package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrUserNotFound is returned when ValidateToken's wrapped existence
// check confirms (via the database) that no User row exists for the
// JWT's `sub`. The error wraps ErrInvalidToken so existing callers
// that switch on ErrInvalidToken keep working without changes — the
// FR-10.1 cleanup contract requires the JWT to be rejected, and the
// existing handler maps that to `auth_failed`.
var ErrUserNotFound = errors.New("user not found")

// ErrUserLookupFailed is returned when the existence check could not be
// ANSWERED — a transport error, a cancelled context, a pool timeout — as
// against answered "no". Both are rejected (the check is fail-closed and stays
// fail-closed), and both still wrap ErrInvalidToken, so every caller that
// branches on that is unaffected.
//
// IT EXISTS BECAUSE THE TWO WERE INDISTINGUISHABLE IN A LOG LINE, and MYR-612
// is what that cost. A participant's `POST /api/trips/{id}/activity-start-token`
// was refused with `trips: invalid token — auth.ValidateToken: user not found:
// userExistenceCache.Exists(user=…)` for an account that plainly existed (and
// whose next two requests, a minute later, were served). The lookup had failed;
// the message said the user was missing, and the incident was diagnosed for
// hours as a go_users-vs-"User" table-ladder bug that had never existed —
// queryUserExists has probed BOTH relations since MYR-369. An operator reading
// "user not found" has no reason to suspect the database.
var ErrUserLookupFailed = errors.New("user existence lookup failed")

// userExistenceTTL is the lifetime of a positive cache entry.
// Per the MYR-73 issue spec, the user-existence check must be cheap
// enough that it can run on every WS handshake without a per-frame DB
// hit. The 1s TTL is the longest acceptable staleness window: a
// deleted user's stale token may pass ValidateToken for at most 1s
// after the deletion commits, then the next call refetches.
const userExistenceTTL = time.Second

// userExistenceLookupTimeout bounds ONE detached existence query. Short,
// because the statement is two primary-key EXISTS probes and every JWT on the
// service waits behind it; long enough that an ordinary pool wait is not
// mistaken for a failure.
const userExistenceLookupTimeout = 3 * time.Second

// userExistenceChecker is the consumer-site interface used by
// userExistenceCache to fetch authoritative existence answers.
// Satisfied by pgUserExistenceQuerier (production) or a stub (tests).
type userExistenceChecker interface {
	UserExists(ctx context.Context, userID string) (bool, error)
}

// userExistenceEntry stores a cached existence answer plus its fetch
// timestamp.
type userExistenceEntry struct {
	exists    bool
	fetchedAt time.Time
}

// userExistenceCache maps userID -> existence answer with a TTL.
// Lookups are singleflight-coalesced so 100 concurrent WS handshakes
// for the same userID fan out to a single DB query.
type userExistenceCache struct {
	checker userExistenceChecker
	entries sync.Map // userID -> *userExistenceEntry
	ttl     time.Duration
	now     func() time.Time
	group   singleflight.Group
}

// newUserExistenceCache constructs a cache backed by checker.
func newUserExistenceCache(checker userExistenceChecker, ttl time.Duration) *userExistenceCache { //nolint:unparam // ttl varies in tests
	if ttl <= 0 {
		ttl = userExistenceTTL
	}
	return &userExistenceCache{
		checker: checker,
		ttl:     ttl,
		now:     time.Now,
	}
}

// Exists returns whether userID has a row in the User table. Cached
// answers (both positive and negative) are reused for up to ttl. A
// transient DB error is propagated to the caller — the JWT path
// treats "lookup failed" as fail-closed (rejected) by wrapping it as
// ErrUserNotFound; we do not silently allow auth on a database
// outage.
func (c *userExistenceCache) Exists(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}

	if entry, ok := c.loadValid(userID); ok {
		return entry.exists, nil
	}

	// THE LOOKUP RUNS ON A DETACHED CONTEXT, and that is MYR-612's fix rather
	// than tidiness.
	//
	// singleflight.Do runs the function under the FIRST caller's context and
	// hands its error to EVERY caller sharing the slot. This is the JWT path,
	// so the contexts here are request contexts, and an iOS client that
	// abandons a request — a view disappearing, a task cancelled on
	// background, a socket dropped — cancels one. Every other caller that
	// arrived in the same millisecond for the same user then received that
	// caller's `context canceled` and, because the check is fail-closed, a
	// 401 for a token that was perfectly good. That is the shape of the
	// MYR-612 incident: one refusal at 03:41:55 for a user whose requests a
	// minute later were served.
	//
	// Detached and re-bounded, the shared lookup belongs to the CACHE rather
	// than to whichever caller happened to open it. It also finishes and
	// populates the entry even when every caller has gone, which is the
	// direction that costs nothing.
	lookup := context.WithoutCancel(ctx)
	val, err, _ := c.group.Do(userID, func() (any, error) {
		// Double-check after acquiring the singleflight slot.
		if entry, ok := c.loadValid(userID); ok {
			return entry.exists, nil
		}
		queryCtx, cancel := context.WithTimeout(lookup, userExistenceLookupTimeout)
		defer cancel()
		exists, err := c.checker.UserExists(queryCtx, userID)
		if err != nil {
			// WRAPPED ONCE, not twice. The caller below used to add a second
			// identical `userExistenceCache.Exists(user=…)` prefix to this same
			// error, so the production line read the frame twice and looked
			// like two different failures stacked on each other.
			return false, fmt.Errorf("userExistenceCache.Exists(user=%s): %w", userID, err)
		}
		c.entries.Store(userID, &userExistenceEntry{
			exists:    exists,
			fetchedAt: c.now(),
		})
		return exists, nil
	})
	if err != nil {
		// Already carries the userExistenceCache.Exists frame from inside the
		// closure — singleflight passes the function's error through verbatim,
		// so wrapping again would repeat the frame, which is exactly the
		// double-wrap MYR-612 removed.
		return false, err //nolint:wrapcheck // wrapped once, inside the singleflight closure
	}
	return val.(bool), nil //nolint:forcetypeassert // singleflight cache only stores bool
}

// Invalidate removes the cached entry for userID. After Invalidate
// returns, the next Exists call refetches from the database. Used by
// the data-lifecycle.md §3.5 cleanup path so a deleted user's
// existence answer flips immediately instead of after the 1s TTL.
func (c *userExistenceCache) Invalidate(userID string) {
	if userID == "" {
		return
	}
	c.entries.Delete(userID)
}

// loadValid returns the cache entry if it exists and has not expired.
func (c *userExistenceCache) loadValid(userID string) (*userExistenceEntry, bool) {
	val, ok := c.entries.Load(userID)
	if !ok {
		return nil, false
	}
	entry := val.(*userExistenceEntry) //nolint:forcetypeassert // cache only stores *userExistenceEntry
	if c.now().Sub(entry.fetchedAt) > c.ttl {
		c.entries.Delete(userID)
		return nil, false
	}
	return entry, true
}

// IsLookupFailure reports whether a ValidateToken error means the existence
// check could not be ANSWERED, as against answered "no".
//
// The transport layers branch on it to choose between 401 (this credential is
// dead — a client may discard the session) and 503 (we could not tell — retry).
// Exported as a predicate rather than as an HTTP mapping so this package keeps
// knowing nothing about HTTP; the three surfaces that authenticate bearers each
// own their own envelope.
func IsLookupFailure(err error) bool { return errors.Is(err, ErrUserLookupFailed) }
