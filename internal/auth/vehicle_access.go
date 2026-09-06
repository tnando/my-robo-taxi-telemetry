package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNoVehicleAccess reports that a caller is neither the owner of a vehicle
// nor the holder of an accepted share on it.
//
// It is a DENIAL, not a lookup failure: handlers map it to
// 403 vehicle_not_owned (or 404, where the endpoint's rule is to keep the
// vehicle's existence hidden), never to a viewer projection and never to a 500.
var ErrNoVehicleAccess = errors.New("no access to vehicle")

// queryShareGrant resolves the CAPABILITY FLAGS one person holds over one
// vehicle through an accepted share (go_vehicle_shares, migrations 0020 +
// 0024). No row means no access — there is deliberately no default grant to
// fall back to.
//
// `suspended_at IS NULL` IS AN ACCESS-CONTROL PREDICATE, not a filter for
// tidiness. A suspended grant must be indistinguishable from no grant on every
// viewer-facing path (MYR-369), so it is excluded HERE, in the statement, next
// to the status check it belongs with — rather than fetched and then discarded
// in Go, which would leave a window for some future caller to read the row and
// act on its flags.
//
// `permission` is deliberately NOT selected. It is the invite-time preset and
// derived output; no gate reads it, and selecting it here would put it within
// reach of one.
const queryShareGrant = `
SELECT allow_rides FROM go_vehicle_shares
WHERE vehicle_id = $1 AND accepted_by_user_id = $2
  AND status = 'accepted' AND suspended_at IS NULL
LIMIT 1`

// rideMembershipLookup is the consumer-site interface ResolveVehicleAccess uses
// to read the MYR-540 group-ride membership source. Separate from shareLookup
// because it is a different KIND of access — ride-scoped and self-expiring
// rather than a standing grant — and keeping the two apart is what stops a
// future edit granting a member a share's capabilities by accident.
type rideMembershipLookup interface {
	// IsRidingVehicle reports whether userID holds a LIVE group-ride membership
	// on a ride being served by vehicleID. Errors are lookup failures, never
	// denials: "no" is (false, nil).
	IsRidingVehicle(ctx context.Context, userID, vehicleID string) (bool, error)
}

// tripParticipationLookup is the consumer-site interface ResolveVehicleAccess
// uses to read the MYR-602 trip-window source. A third KIND of access again,
// kept apart from the other two for the same reason they are kept apart from
// each other: a window-scoped grant and a standing one must not be able to
// acquire each other's capabilities through a shared code path.
type tripParticipationLookup interface {
	// IsActiveTripParticipant reports whether userID is a live participant of a
	// trip on vehicleID whose window is OPEN RIGHT NOW. Errors are lookup
	// failures, never denials: "no" is (false, nil).
	IsActiveTripParticipant(ctx context.Context, userID, vehicleID string) (bool, error)
}

// shareLookup is the consumer-site interface ResolveVehicleAccess uses to read
// an accepted share grant. Defined here so tests can swap the DB-backed
// implementation for a stub, mirroring vehicleOwnerLookup.
type shareLookup interface {
	// GetShareGrant returns the capability set userID holds over vehicleID.
	// Returns ErrNoVehicleAccess when there is no accepted grant OR when the
	// grant exists but is suspended — the two are deliberately the same
	// answer.
	GetShareGrant(ctx context.Context, userID, vehicleID string) (ShareGrant, error)
}

// ResolveVehicleAccess resolves the caller's role for a vehicle AND, for every
// non-owner role, the capability set their share grant carries.
//
// FIVE outcomes since MYR-602, resolved STRONGEST FIRST:
//
//   - owner            — (RoleOwner, ShareGrant{}, nil). An owner holds no
//     grant; they hold everything. The returned grant is the ZERO VALUE, which
//     is the most restrictive one, so a caller that forgets to branch on the
//     role denies rather than over-grants. Branch on the role first.
//   - trip_participant — (RoleTripParticipant, grant, nil) inside an open trip
//     window. `grant` is the caller's own share grant, which they still hold —
//     the trip elevates the ROLE, it does not replace the relationship.
//   - ride_member      — (RoleRideMember, grant, nil) while riding.
//   - viewer           — (RoleViewer, grant, nil) on a live accepted share with
//     no window open. NARROWED BY MYR-602: this role no longer receives the
//     location or navigation groups. See internal/mask/tables.go.
//   - denied           — (Role(""), ShareGrant{}, ErrNoVehicleAccess).
//
// THE ELEVATED SOURCES ARE PROBED BEFORE THE SHARE ANSWERS, and that ordering
// is the whole correctness of MYR-602's narrowing. Every trip participant is
// by construction also a share-holder, so resolving the share first — as this
// function did before — would return RoleViewer for every participant and the
// window would grant nothing at all.
//
// A SUSPENDED GRANT IS THE DENIED CASE, not a viewer with an empty capability
// set (MYR-369). The distinction matters: a suspended grant must be
// indistinguishable from no grant at all on every viewer-facing surface, and
// returning RoleViewer for one would put the caller inside the viewer branch of
// every handler — masked, but present.
//
// A vehicle that does not exist surfaces as an error from the owner lookup, NOT
// as a denial, so the handler layer can answer 404 rather than 403 — the
// existing rule that an unknown vehicle must never be distinguishable from one
// the caller merely cannot see is enforced at that layer, where the correct
// status for each endpoint is known.
func (a *JWTAuthenticator) ResolveVehicleAccess(ctx context.Context, userID, vehicleID string) (Role, ShareGrant, error) {
	ownerID, err := a.ownerLookup.GetVehicleOwnerByID(ctx, vehicleID)
	if err != nil {
		return Role(""), ShareGrant{}, fmt.Errorf("auth.ResolveVehicleAccess: vehicle %s: %w", vehicleID, err)
	}
	if ownerID == userID {
		return RoleOwner, ShareGrant{}, nil
	}

	// The grant is read FIRST but no longer decides the role on its own. It is
	// read first because it is the CAPABILITY carrier: a share-holder who is
	// also on a trip keeps allow_rides, and resolving the elevated role without
	// this read would silently downgrade them to the zero-value grant for the
	// length of the window.
	//
	// `held` IS NOT DERIVABLE FROM `grant`, and that is the whole reason it is
	// returned separately. ShareGrant's zero value has Suspended=false, so
	// ShareGrant{}.Active() is TRUE — "no grant at all" and "a live grant with
	// no flags set" are the same bytes. Before MYR-602 the distinction was
	// carried by GetShareGrant's ERROR, which the elevated probes now have to
	// run past; collapsing it into the zero value would admit every
	// authenticated stranger as a viewer on every vehicle.
	grant, held, shareErr := a.shareGrant(ctx, userID, vehicleID)
	if shareErr != nil {
		return Role(""), ShareGrant{}, shareErr
	}

	// THE ELEVATED SOURCES, PROBED BEFORE THE SHARE IS ALLOWED TO ANSWER.
	//
	// Order between these two does not affect the field set — the two roles
	// share one allow-list by construction (see auth.LiveLocationRoles) — so
	// what the order picks is the PROVENANCE reported to the handlers, and
	// trip participation is the more specific of the two (it is what admits a
	// caller to the window's drives; membership admits them to none).
	if a.tripParticipant(ctx, userID, vehicleID) {
		return RoleTripParticipant, grant, nil
	}
	if a.ridingMember(ctx, userID, vehicleID) {
		return RoleRideMember, grant, nil
	}

	if !held || !grant.Active() {
		// TWO INDEPENDENT DENIALS SHARING ONE ANSWER, which is the point: a
		// suspended grant must be indistinguishable from no grant at all
		// (MYR-369).
		//
		// The `!held` arm is the real gate for a stranger. The `!grant.Active()`
		// arm is the SECOND, INDEPENDENT SUSPENSION GATE: the statement already
		// excludes suspended rows, so it cannot fire through the DB-backed
		// lookup — it fires for a stub, a future lookup implementation, or a
		// statement somebody edits. An access-control invariant that holds only
		// because one WHERE clause is correct is one edit from not holding.
		return Role(""), ShareGrant{}, ErrNoVehicleAccess
	}
	return RoleViewer, grant, nil
}

// shareGrant reads the accepted grant, reporting "no grant" as (zero, false,
// nil) rather than as an error so the caller can go on to probe the elevated
// sources.
//
// THE BOOLEAN IS LOAD-BEARING and cannot be inferred from the grant: see the
// `held` paragraph in ResolveVehicleAccess.
//
// A TRANSPORT failure is still an error: a database blip must not be reported
// as an absent grant, because that would silently strip a share-holder of the
// capabilities the ride gates read from it — and, symmetrically, must not be
// reported as a PRESENT one.
func (a *JWTAuthenticator) shareGrant(ctx context.Context, userID, vehicleID string) (ShareGrant, bool, error) {
	if a.shares == nil {
		// No share lookup configured — fail closed. The pre-MYR-184 behaviour
		// of returning RoleViewer here is exactly the hole that change closed.
		// The elevated probes still run: a ride member or trip participant
		// needs no share lookup to be resolved, and denying them here would
		// make an unwired share source silently disable group rides too.
		return ShareGrant{}, false, nil
	}
	grant, err := a.shares.GetShareGrant(ctx, userID, vehicleID)
	switch {
	case err == nil:
		return grant, true, nil
	case errors.Is(err, ErrNoVehicleAccess):
		return ShareGrant{}, false, nil
	default:
		return ShareGrant{}, false, fmt.Errorf("auth.ResolveVehicleAccess(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
}

// ridingMember is the MYR-540 GROUP-RIDE MEMBERSHIP probe.
//
// FAILS CLOSED, and it does so by returning false rather than an error: this
// probe runs on a path that has a correct answer without it (the share tier, or
// a denial), so a database blip here must not convert a request into a 500. A
// genuine member whose probe failed is served the narrower role for this
// request and the elevated one on the retry, which is the same bound the
// 5-minute access-set cache already imposes.
func (a *JWTAuthenticator) ridingMember(ctx context.Context, userID, vehicleID string) bool {
	if a.rides == nil {
		return false
	}
	riding, err := a.rides.IsRidingVehicle(ctx, userID, vehicleID)
	return err == nil && riding
}

// tripParticipant is the MYR-602 TRIP-WINDOW probe, with the same fail-closed
// posture and for the same reason as ridingMember.
//
// WINDOW-ONLY. It asks whether the clock is inside an open window on a live
// membership backed by a live share — never whether a leg is underway, whether
// the car is moving, or whether it has a destination. A parked car inside the
// window resolves RoleTripParticipant and streams its position, which is the
// stated product behaviour (client ruling, 2026-09-05).
func (a *JWTAuthenticator) tripParticipant(ctx context.Context, userID, vehicleID string) bool {
	if a.trips == nil {
		return false
	}
	participant, err := a.trips.IsActiveTripParticipant(ctx, userID, vehicleID)
	return err == nil && participant
}

// IsActiveTripParticipant reports whether userID is a live participant of an
// OPEN trip window on vehicleID (MYR-602). A closed window matches nothing,
// which is how trip access ends without anything having to revoke it.
func (q *pgVehicleQuerier) IsActiveTripParticipant(ctx context.Context, userID, vehicleID string) (bool, error) {
	var participant bool
	if err := q.pool.QueryRow(ctx, queryActiveTripParticipation, userID, vehicleID).Scan(&participant); err != nil {
		return false, fmt.Errorf("pgVehicleQuerier.IsActiveTripParticipant(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
	return participant, nil
}

// IsRidingVehicle reports whether userID is a member of a LIVE group ride being
// served by vehicleID (MYR-540). A terminal ride matches nothing, which is how
// membership access ends without anything having to revoke it.
func (q *pgVehicleQuerier) IsRidingVehicle(ctx context.Context, userID, vehicleID string) (bool, error) {
	var riding bool
	if err := q.pool.QueryRow(ctx, queryRideMembershipOnVehicle, userID, vehicleID).Scan(&riding); err != nil {
		return false, fmt.Errorf("pgVehicleQuerier.IsRidingVehicle(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
	return riding, nil
}

// InvalidateVehicles drops the cached access set for userID so the next
// GetUserVehicles refetches from the database.
//
// CACHE POLICY (MYR-184). The access set is cached for vehicleCacheTTL, so a
// share that appears or disappears would otherwise take up to that long to be
// visible. The two transitions are treated differently, deliberately:
//
//   - REDEEM busts the REDEEMER's entry, so the car they just joined shows up
//     in their list on the very next request rather than minutes later. A
//     "you're in!" screen followed by an empty garage is the feature failing.
//   - REVOKE busts the REVOKED VIEWER's entry, so access ends promptly. This
//     one is a security property, not a niceness: a bounded window where a
//     revoked viewer still resolves the vehicle is a real (if brief) exposure.
//   - PATCH busts the GRANTEE's entry (MYR-369), for the same reason as revoke
//     and with the same urgency. A suspension REMOVES the vehicle from that
//     person's access set, so it is a revoke in every respect that matters to
//     this cache; leaving the entry warm would let a suspended viewer keep
//     resolving the car for up to the TTL, which is the exact exposure the
//     revoke bust exists to prevent. The un-suspend and the allow_rides
//     directions are busted by the same call — not because they are dangerous
//     (they widen or narrow one capability, and the ride gates read the row
//     directly rather than the cache) but because ONE unconditional bust on
//     mutation is a rule that cannot be got wrong, and a bust conditional on
//     which field moved is a rule that can.
//
// What this does NOT do is make the cache authoritative across processes. The
// entry is per-instance, so on a multi-machine deployment a bust only clears
// the machine that served the mutation and the others still lapse on TTL. The
// app runs a single Fly machine today (fly.toml declares one [[vm]] with no
// scale-out), so the bust is complete in practice; if that changes, revocation
// needs a cross-instance signal (the LISTEN/NOTIFY channel the user-deletion
// path already uses is the obvious carrier).
func (a *JWTAuthenticator) InvalidateVehicles(userID string) {
	// Nil-receiver tolerant: dev mode has no JWTAuthenticator, and a
	// cache-bust that silently does nothing is strictly better than a panic
	// on a path whose whole job is housekeeping.
	if a == nil || a.cache == nil || userID == "" {
		return
	}
	a.cache.invalidate(userID)
}

// GetShareGrant reads a LIVE accepted share grant from go_vehicle_shares.
// Returns ErrNoVehicleAccess when there is none — which covers "no grant",
// "revoked" and "suspended" alike, because the statement's WHERE clause makes
// all three produce zero rows.
//
// Suspended is therefore always false on a grant this returns. It is set on the
// returned struct anyway (rather than left implicitly zero) nowhere at all: the
// row simply does not come back, which is the stronger property.
func (q *pgVehicleQuerier) GetShareGrant(ctx context.Context, userID, vehicleID string) (ShareGrant, error) {
	var allowRides bool
	err := q.pool.QueryRow(ctx, queryShareGrant, vehicleID, userID).Scan(&allowRides)
	switch {
	case err == nil:
		return ShareGrant{AllowRides: allowRides}, nil
	case errors.Is(err, pgx.ErrNoRows):
		return ShareGrant{}, ErrNoVehicleAccess
	default:
		return ShareGrant{}, fmt.Errorf("pgVehicleQuerier.GetShareGrant(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
}
