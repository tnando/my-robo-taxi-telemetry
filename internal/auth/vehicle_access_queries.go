package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The pgVehicleQuerier methods behind the three non-owner probes.
//
// Split from vehicle_access.go so both stay inside the 300-line cap, and along
// the seam the file already had: that one is the RESOLUTION — the order the
// four roles are tried in, and what each of them means — while these are the
// statements it runs, one per probe. The order is the security property and it
// should be readable without scrolling past three scanners.
//
// EVERY ONE OF THEM FAILS CLOSED at the call site rather than here: the
// resolver treats a non-nil error as "no", so a database blip narrows a caller
// instead of widening them. See vehicle_access.go.

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
