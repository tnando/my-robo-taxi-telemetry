package ws

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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
// and closes sessions that are holding a vehicle the user can no longer see
// (MYR-373, websocket-protocol.md §10 DV-09) — and, since MYR-602, RE-MASKS in
// place the sessions whose access changed TIER rather than disappearing.
//
// THE SECOND JOB IS WHAT TRIPS NEEDED. A share is revoked by somebody clicking
// something, so there is a mutation to hang the fast nudge off. A TRIP WINDOW
// OPENS AND CLOSES ON THE CLOCK: nothing is written at `starts_at`, nobody
// calls anything at `ends_at`, and the only thing that has changed a
// millisecond later is what `NOW()` returns inside the access query. So this
// sweep is not a backstop for the window case — it IS the mechanism, and its
// interval is the whole latency budget (~60s, matching the trip sweeper's).
//
// Two directions, and both are real:
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
// A KICK STILL BEATS A RE-MASK. The lost-vehicle check runs first and returns:
// a session that must be closed is closed, and re-masking a client that is
// about to be torn down would be work for a socket with no future.
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
// it closed. Exported so a test can drive one deterministic pass instead of
// waiting on the ticker.
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
	closed, remasked := 0, 0

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
		// MYR-602 — the access set is intact; is the TIER? Only reached for a
		// session that is staying open, and only for the vehicles it actually
		// holds.
		if r.remaskClient(ctx, client) {
			remasked++
		}
	}

	// Only when it actually fired. A backstop that closes something is a
	// signal worth seeing — either the nudge did not reach this hub, or a
	// mutation happened somewhere that does not publish — and logging every
	// quiet pass at Info would bury exactly that line once a minute forever.
	if closed > 0 || remasked > 0 {
		r.logger.Info("access revalidation swept",
			slog.Int("sessions_closed", closed),
			// MYR-602: a re-mask is the ORDINARY outcome at a trip window edge,
			// unlike a close, which always means the nudge failed to arrive.
			// Both are on one line so an operator reading it can tell a window
			// opening from a grant being pulled.
			slog.Int("sessions_remasked", remasked),
			slog.Int("clients_examined", len(clients)),
		)
	}
	return closed
}

// remaskClient re-resolves this session's per-vehicle roles and republishes the
// table when any of them moved. Reports whether it republished.
//
// THE WHOLE TABLE IS REPLACED, never edited: Client.roles is read lock-free on
// the broadcast hot path, and the immutability of a published table is what
// makes that safe. See Client.setRoles.
//
// IT RESOLVES ONLY THE VEHICLES THE CLIENT HOLDS, and it does so one at a time
// on the sweep's single goroutine — the same shape as the access read above.
// The cost is one ResolveRole per (session, vehicle) per minute, which at this
// fleet's size is a handful of indexed reads; if it ever stops being, the fix
// is a batch resolver, not a wider interval, because the interval IS the
// window-edge latency.
//
// A FAILED RESOLUTION KEEPS THE OLD ROLE for that vehicle and does NOT abandon
// the pass. Failing open is right in both directions here and neither is a
// judgement call: keeping the old role means a narrowing is delayed by one
// interval (bounded, and the same bound the whole sweep already has), while
// falling back to the deny-all sentinel would black out a live map on a
// database blip, and falling back to the strongest role would hand out location
// on one. Keeping what was already true is the only answer that invents
// nothing.
func (r *AccessRevalidator) remaskClient(ctx context.Context, client *Client) bool {
	current := client.rolesSnapshot()
	if len(current) == 0 {
		// Nothing published: either a dev-mode wildcard client (whose role
		// comes from defaultRole and is not per-vehicle) or a handshake whose
		// every ResolveRole failed. Neither has a table to narrow, and minting
		// one here would be this sweep deciding a role the handshake refused
		// to.
		return false
	}

	next := maps.Clone(current)
	changed := false
	for vehicleID, held := range current {
		role, err := r.resolveRole(ctx, client.userID, vehicleID)
		if err != nil {
			r.logger.Warn("access revalidation: role resolve failed; keeping the current role",
				slog.String("user_id", client.userID),
				slog.String("vehicle_id", vehicleID),
				slog.String("role", held.String()),
				slog.Any("error", err),
			)
			continue
		}
		if role == held {
			continue
		}
		next[vehicleID] = role
		changed = true
		r.logger.Info("access revalidation: role changed mid-connection",
			slog.String("user_id", client.userID),
			slog.String("vehicle_id", vehicleID),
			slog.String("from", held.String()),
			slog.String("to", role.String()),
		)
	}
	if !changed {
		return false
	}
	client.setRoles(next)
	return true
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

// accessSet is one user's current entitlement.
//
// The `all` flag is why this is a struct and not a bare map. "All access" and
// "no access" both produce zero per-vehicle entries, and they are opposite
// answers: the first must close nothing, the second must close everything that
// user has open. A bare map would have to encode the difference as nil-versus-
// empty, which is exactly the distinction a future edit drops.
type accessSet struct {
	// all is the dev-mode wildcard sentinel: authorized for every vehicle.
	all bool
	// allowed holds the concrete vehicle ids. Empty and non-nil is a real
	// answer — the user may see nothing.
	allowed map[string]struct{}
}

// resolveTimeout caps a single resolver call. The sweep walks every connected
// user on one goroutine, so a hung database read would wedge the whole pass
// and, with it, every later pass — the backstop would go quiet exactly when
// the thing it backstops is most likely to be struggling. Nothing on the path
// today blocks unboundedly; this is the guard that keeps that true.
const resolveTimeout = 5 * time.Second

// resolve fetches the user's current entitlement.
func (r *AccessRevalidator) resolve(ctx context.Context, userID string) (accessSet, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	ids, err := r.resolver.GetUserVehicles(ctx, userID)
	if err != nil {
		return accessSet{}, fmt.Errorf("revalidator.resolve(user=%s): %w", userID, err)
	}
	out := accessSet{allowed: make(map[string]struct{}, len(ids))}
	for _, id := range ids {
		if id == WildcardVehicleID {
			return accessSet{all: true}, nil
		}
		out.allowed[id] = struct{}{}
	}
	return out, nil
}

// firstLostVehicle reports a vehicle the client is still holding that the
// user's current entitlement no longer covers. One is enough: RevokeUserAccess
// closes the whole session, so finding a second would change nothing.
func firstLostVehicle(c *Client, set accessSet) (string, bool) {
	if set.all {
		return "", false
	}
	for _, held := range c.authorizedVehicles() {
		if _, ok := set.allowed[held]; !ok {
			return held, true
		}
	}
	return "", false
}
