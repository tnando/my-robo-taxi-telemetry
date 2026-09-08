package ws

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// DefaultRevalidateInterval is how often the backstop sweep re-derives every
// connected user's access set. It is a BACKSTOP, not the mechanism: the
// event-driven nudge (hub_access.go RevokeUserAccess, driven by the share
// mutation path) is what actually makes a revocation take effect, and it does
// so in milliseconds. This sweep exists because that nudge is not a delivery
// guarantee.
//
// Specifically, the in-process bus drops the OLDEST event when a subscriber's
// buffer is full (internal/events/subscriber.go), the publishing handler and
// the hub can only agree in-process so a mutation served by a different
// machine reaches this hub's sockets through nothing at all, and any future
// mutation path that forgets to publish would silently reopen DV-09. A
// security property that depends on every caller remembering to announce it is
// not a security property. This sweep re-derives from the database and is
// therefore correct regardless of who mutated what, where.
//
// SINCE MYR-601 THE SAME ARGUMENT COVERS THE WIDENING DIRECTION, and it had to:
// every one of those failure modes exists there too, plus one that has no
// event-driven answer at all — the Next.js app writes `"Vehicle"` rows straight
// into the shared database (react-frontend `sync.ts`), reaching neither this
// process's access cache nor its bus. Nothing can publish for a writer in
// another process. Re-deriving from the database is what makes the ≤60s bound
// a property of the DATA rather than of every writer remembering the rule.
const DefaultRevalidateInterval = 60 * time.Second

// AccessResolver re-derives a user's authorized vehicle set AND their role on
// each vehicle in it. Defined at the consumer site; satisfied by the same
// *auth.JWTAuthenticator the handshake already uses, so the sweep and the
// handshake cannot disagree about what a user may see — nor, since MYR-602,
// about which TIER they see it at.
//
// Note the cache underneath: JWTAuthenticator serves this from a 5-minute
// per-process cache. On the machine that served the mutation the entry was
// busted synchronously, so the sweep reads fresh and the backstop bound is one
// interval. On any OTHER machine the entry lapses on its own TTL, so the bound
// there is the TTL plus one interval. Both are bounded; only the first is fast.
type AccessResolver interface {
	GetUserVehicles(ctx context.Context, userID string) ([]string, error)
	// ResolveRole re-derives one (user, vehicle) role. Same method the
	// handshake calls, so a window edge resolves identically whether the
	// caller reconnected or was re-masked in place.
	ResolveRole(ctx context.Context, userID, vehicleID string) (auth.Role, error)
}

// AccessRevalidator periodically re-derives every connected user's access set
// and reconciles every connected session against it. THREE ARMS, added in the
// order the gaps were found:
//
//   - CLOSE a session holding a vehicle the user can no longer see (MYR-373,
//     websocket-protocol.md §10 DV-09).
//   - CLOSE a session that is missing a vehicle the user CAN now see, so the
//     reconnect picks it up (MYR-601). This is what makes the ≤60s bound true
//     in the widening direction, including for writers outside this process.
//     See access_widen_backstop.go.
//   - RE-MASK in place the sessions whose access changed TIER rather than
//     appearing or disappearing (MYR-602). See access_remask.go.
//
// THE SECOND JOB IS WHAT TRIPS NEEDED. A share is revoked by somebody clicking
// something, so there is a mutation to hang the fast nudge off. A TRIP WINDOW
// OPENS AND CLOSES ON THE CLOCK: nothing is written at `starts_at`, nobody
// calls anything at `ends_at`, and the only thing that has changed a
// millisecond later is what `NOW()` returns inside the access query. So this
// sweep is not a backstop for the window case — it IS the mechanism, and its
// interval is the whole latency budget (~60s, matching the trip sweeper's).
//
// Two directions of the TIER change, and both are real:
//
//   - NARROWING. A participant whose window just closed still holds their
//     accepted share, so the VEHICLE stays in their access set and the old
//     kick-on-loss path never fires. Without a role re-resolution they would
//     keep receiving the live GPS the window was the entire justification for,
//     for as long as they left the socket open. They are re-masked down to
//     `viewer` instead — the connection survives, the location stops.
//   - WIDENING. A share-holder whose window just opened is already connected as
//     a plain `viewer`, and their frozen handshake role would have kept them
//     there until they happened to reconnect. They are promoted to
//     `trip_participant` in place, which is what makes "the trip started and my
//     map came alive" true without the app having to reconnect on a timer.
//
// A KICK STILL BEATS A RE-MASK, and so does a widen. The lost-vehicle check
// runs first and returns, then the gained-vehicle one: a session that is about
// to be torn down is torn down, and re-masking a socket with no future would be
// work for nobody. A client that both lost and gained is closed once, by the
// loss, and its reconnect resolves both.
type AccessRevalidator struct {
	hub      *Hub
	resolver AccessResolver
	interval time.Duration
	logger   *slog.Logger
}

// NewAccessRevalidator builds the backstop sweep. A non-positive interval
// falls back to DefaultRevalidateInterval.
func NewAccessRevalidator(hub *Hub, resolver AccessResolver, interval time.Duration, logger *slog.Logger) *AccessRevalidator {
	if interval <= 0 {
		interval = DefaultRevalidateInterval
	}
	return &AccessRevalidator{hub: hub, resolver: resolver, interval: interval, logger: logger}
}

// Run sweeps every interval until ctx is cancelled. Intended to be started as
// a goroutine at wiring time. It does NOT sweep immediately on start: at
// startup there are no connections to revalidate, and a sweep racing the first
// handshakes buys nothing.
func (r *AccessRevalidator) Run(ctx context.Context) {
	if r.hub == nil || r.resolver == nil {
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.SweepOnce(ctx)
		}
	}
}

// SweepOnce runs a single revalidation pass and returns the number of sessions
// it closed — narrowings and widenings together, since both end a session; the
// pass's own log line is where they are told apart. Exported so a test can
// drive one deterministic pass instead of waiting on the ticker.
func (r *AccessRevalidator) SweepOnce(ctx context.Context) int {
	clients := r.hub.snapshotClients()
	if len(clients) == 0 {
		return 0
	}

	// One resolver call per distinct USER, not per connection: a user with
	// three tabs open is one access set, and the handshake cache would serve
	// the other two from memory anyway. Resolving once also guarantees every
	// session of that user is judged against the SAME answer, so a sweep
	// cannot close one tab and spare another on a cache expiry landing
	// mid-loop.
	resolved := make(map[string]accessSet, len(clients))
	// AND ONE ROLE RESOLUTION PER (USER, VEHICLE), for the same two reasons the
	// access memo above gives. A user with three tabs open holds one role on
	// each car, and resolving once guarantees every session of that user is
	// re-masked to the SAME answer — so a pass cannot narrow one tab and leave
	// another elevated because a cache entry lapsed mid-loop. It also bounds
	// the pass's database work by (distinct users × their cars) rather than by
	// connections, which is what keeps the 60-second interval — the whole
	// window-edge latency budget — affordable.
	roles := make(map[userVehicle]auth.Role, len(clients))
	// AND ONE WIDENING PER USER PER PASS (MYR-601). Unlike a narrowing, which
	// is keyed on the session holding the lost car, a widening closes EVERY
	// session that user holds — so a second tab would publish a second close
	// for sessions the first one already ended. See widenUser.
	announced := make(map[string]struct{})
	closed, widened, remasked := 0, 0, 0

	for _, client := range clients {
		// Dev-mode wildcard clients are authorized for everything by
		// construction; there is no set to narrow.
		if client.allVehicles || client.userID == "" {
			continue
		}

		allowed, ok := resolved[client.userID]
		if !ok {
			var err error
			allowed, err = r.resolve(ctx, client.userID)
			if err != nil {
				// FAIL OPEN, DELIBERATELY. A database blip must not
				// disconnect every connected client at once; the
				// event-driven nudge is the mechanism and this is the
				// backstop, so a skipped pass costs one interval of
				// staleness while a fail-closed sweep would turn a
				// transient query error into a fleet-wide outage.
				r.logger.Warn("access revalidation skipped: resolve failed",
					slog.String("user_id", client.userID),
					slog.Any("error", err),
				)
				continue
			}
			resolved[client.userID] = allowed
		}

		if lost, found := firstLostVehicle(client, allowed); found {
			closed += r.hub.RevokeUserAccess(client.userID, lost, "revalidation_backstop")
			continue
		}
		// MYR-601 — and the other direction, which until then had event-driven
		// producers and no backstop at all: this user may now see a car this
		// SESSION was never authorized for. Re-handshake it, which is what makes
		// the documented ≤60s bound true for a widening that nobody announced —
		// including one written by a producer outside this process. See
		// access_widen_backstop.go.
		if gained, found := firstGainedVehicle(client, allowed); found {
			widened += r.widenUser(client, gained, announced)
			continue
		}
		// MYR-602 — the access set is intact; is the TIER? Only reached for a
		// session that is staying open, and only for the vehicles it actually
		// holds.
		if r.remaskClient(ctx, client, roles) {
			remasked++
		}
	}

	// Only when it actually fired. A backstop that closes something is a
	// signal worth seeing — either the nudge did not reach this hub, or a
	// mutation happened somewhere that does not publish — and logging every
	// quiet pass at Info would bury exactly that line once a minute forever.
	if closed > 0 || widened > 0 || remasked > 0 {
		r.logger.Info("access revalidation swept",
			slog.Int("sessions_closed", closed),
			// MYR-601: a widening reaching the BACKSTOP means the same thing a
			// close does — nothing announced it — but in the harmless direction,
			// so it is counted apart from the narrowings rather than folded into
			// them. The two must not share a number for the same reason the two
			// bus topics do not.
			slog.Int("sessions_widened", widened),
			// MYR-602: a re-mask is the ORDINARY outcome at a trip window edge,
			// unlike a close, which always means the nudge failed to arrive.
			// Both are on one line so an operator reading it can tell a window
			// opening from a grant being pulled.
			slog.Int("sessions_remasked", remasked),
			slog.Int("clients_examined", len(clients)),
		)
	}
	// Both arms END SESSIONS, and the caller's one number is "how many sockets
	// did this pass close". A widening closes them to hand the user more; a
	// narrowing to take something away. The log line above is where the two are
	// told apart.
	return closed + widened
}

// resolveRole fetches one (user, vehicle) role under the same timeout the
// access read uses, for the same reason: the sweep walks every connected
// session on one goroutine, and a hung read would wedge every later pass.
func (r *AccessRevalidator) resolveRole(ctx context.Context, userID, vehicleID string) (auth.Role, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	role, err := r.resolver.ResolveRole(ctx, userID, vehicleID)
	if err != nil {
		return auth.Role(""), fmt.Errorf("revalidator.resolveRole(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
	return role, nil
}
