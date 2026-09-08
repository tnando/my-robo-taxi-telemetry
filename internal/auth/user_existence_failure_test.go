package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// MYR-612 — the four properties the production incident turned on.
//
// A participant's `POST /api/trips/{tripId}/activity-start-token` was refused
// with `auth.ValidateToken: user not found: userExistenceCache.Exists(user=…)`
// for an account that existed in the Prisma "User" table, and whose next two
// requests a minute later were served. The row was never missing; the LOOKUP
// failed, and the message could not say so.

// TestQueryUserExistsProbesBothRelations pins the ladder itself.
//
// It is a text assertion rather than a database one deliberately: the two
// relations are the whole contract of the statement — a pre-identity-module
// account exists ONLY in "User", an Apple-native one exists ONLY in go_users —
// and a probe that dropped either would reject half the fleet with a message
// that says the account is gone. The DB-level proof of the same ladder lives in
// internal/identity's TestE2E_DualAlgWithGoUsersExistence, which runs the real
// statement against a real Postgres.
func TestQueryUserExistsProbesBothRelations(t *testing.T) {
	for _, relation := range []string{`FROM "User"`, `FROM go_users`} {
		if !strings.Contains(queryUserExists, relation) {
			t.Errorf("queryUserExists must probe %s; a caller present in only one relation would be rejected:\n%s",
				relation, queryUserExists)
		}
	}
}

// TestLookupFailureIsNotUserNotFound is the message the incident needed.
func TestLookupFailureIsNotUserNotFound(t *testing.T) {
	boom := errors.New("timeout: context deadline exceeded")
	a := &JWTAuthenticator{
		secret:          []byte(testSecret),
		userExistsCache: newUserExistenceCache(&fakeChecker{err: boom}, time.Hour),
	}
	token := signToken(t, testSecret, jwt.MapClaims{
		"sub": "live-user", "exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := a.ValidateToken(context.Background(), token)
	if err == nil {
		t.Fatal("a lookup that could not be answered must still be refused (fail-closed)")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("still wraps ErrInvalidToken, got %v", err)
	}
	if !errors.Is(err, ErrUserLookupFailed) {
		t.Errorf("want ErrUserLookupFailed, got %v", err)
	}
	if errors.Is(err, ErrUserNotFound) {
		t.Errorf("a failed lookup must NOT claim the user is missing: %v", err)
	}
	if !IsLookupFailure(err) {
		t.Errorf("IsLookupFailure must recognise it so transports can answer 503")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying cause must stay in the chain: %v", err)
	}
	// Wrapped ONCE. The production line repeated the frame and read as two
	// stacked failures.
	if n := strings.Count(err.Error(), "userExistenceCache.Exists(user="); n != 1 {
		t.Errorf("existence frame appears %d times, want 1: %v", n, err)
	}
}

// TestLookupErrorIsNotCached asserts a failed probe leaves NO entry behind — a
// cached negative would turn one hiccup into a full TTL of 401s.
func TestLookupErrorIsNotCached(t *testing.T) {
	checker := &fakeChecker{err: errors.New("boom")}
	c := newUserExistenceCache(checker, time.Hour)

	if _, err := c.Exists(context.Background(), "u"); err == nil {
		t.Fatal("want error")
	}
	if _, ok := c.entries.Load("u"); ok {
		t.Fatal("a failed lookup must not store an answer")
	}
	checker.err = nil
	checker.existsBy = map[string]bool{"u": true}
	exists, err := c.Exists(context.Background(), "u")
	if err != nil || !exists {
		t.Fatalf("the next call must re-probe and succeed: exists=%v err=%v", exists, err)
	}
}

// TestCancelledPeerDoesNotFailAHealthyCaller is the MYR-612 mechanism itself.
//
// singleflight runs the shared function under the FIRST caller's context and
// hands its error to everybody in the slot. On the JWT path those are REQUEST
// contexts: an iOS client abandoning a request cancelled one, and every caller
// that arrived in the same millisecond for the same account was refused with
// that caller's `context canceled` — a 401 for a token that was fine.
func TestCancelledPeerDoesNotFailAHealthyCaller(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	checker := &blockingChecker{entered: entered, release: release}
	c := newUserExistenceCache(checker, time.Hour)

	first, cancelFirst := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = c.Exists(first, "u") // the caller that goes away
	}()

	<-entered // the shared lookup is in flight under the first caller's ctx

	second := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.Exists(context.Background(), "u")
		second <- err
	}()

	cancelFirst()
	close(release)
	wg.Wait()

	if err := <-second; err != nil {
		t.Fatalf("a healthy caller must not inherit a peer's cancellation: %v", err)
	}
	if ctxErr := checker.observedErr(); ctxErr != nil {
		t.Errorf("the shared lookup ran on a cancelled context: %v", ctxErr)
	}
}

// blockingChecker parks inside UserExists until released, and records whether
// the context it was handed had been cancelled by then.
type blockingChecker struct {
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	ctxError error
}

func (b *blockingChecker) UserExists(ctx context.Context, _ string) (bool, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	b.mu.Lock()
	b.ctxError = ctx.Err()
	b.mu.Unlock()
	return true, nil
}

func (b *blockingChecker) observedErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctxError
}
