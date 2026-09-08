package store

import (
	"context"
	"fmt"
	"time"
)

// The MYR-602 CATALOG and PUSH-TO-START TOKEN reads.
//
// GET /api/vehicles gains a third merge leg — the vehicles of the caller's open
// windows — and every non-owner row plus every owner row gains `activeTripId`.
// The two are separate reads because they answer different questions: one adds
// ROWS, the other annotates rows that are already there.

// ActiveTripVehicleIDs returns the vehicles of the caller's OPEN windows, each
// with the id of the trip that opens it.
//
// PARTICIPANT ROWS ONLY, and that is right for THIS question: an owner's own
// cars are the first leg of the catalog already, so re-emitting them here would
// produce duplicates the merge discards anyway.
//
// ⚠ IT IS THE WRONG ANSWER TO THE OTHER QUESTION, which is what MYR-612 turned
// on. "Which cars does a trip ADD to your catalog" and "which of your cars have
// a window open right now" are different sets, and an owner's own car is in the
// second and never in the first. ActiveTripIDsForUser answers that one; do not
// point it back here.
//
// Returned as a map because the caller's next move is a lookup, and because a
// caller on two open trips on the SAME car (legal — two owners cannot both own
// it, but one owner may run overlapping trips only if... they cannot; the
// overlap probe forbids it) still yields one entry. The DISTINCT in the
// statement makes that structural rather than incidental.
func (r *TripRepo) ActiveTripVehicleIDs(ctx context.Context, userID string) (map[string]string, error) {
	const op = "trip.active_vehicle_ids"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	rows, err := r.pool.Query(ctx, queryActiveTripVehicleIDsForUser, userID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.ActiveTripVehicleIDs(%s): %w", userID, err)
	}
	defer rows.Close()

	out := make(map[string]string, 2)
	for rows.Next() {
		var vehicleID, tripID string
		if err := rows.Scan(&vehicleID, &tripID); err != nil {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.ActiveTripVehicleIDs(%s): scan: %w", userID, err)
		}
		if _, seen := out[vehicleID]; !seen {
			out[vehicleID] = tripID
		}
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.ActiveTripVehicleIDs(%s): rows: %w", userID, err)
	}
	return out, nil
}

// ActiveTripIDsForUser returns vehicleID → tripID for EVERY open window the
// caller is party to, INCLUDING the ones on cars they own.
//
// THE OWNER ARM IS THE POINT (MYR-612). This used to be served by
// ActiveTripVehicleIDs above — the participant-only merge leg — so an owner's
// own car never carried `activeTripId`. The iOS client registers its ActivityKit
// push-to-start token for the trips it reads out of the catalog, so the owner of
// the car on the trip registered nothing and got no leg card on their own trip.
// The two questions look alike and are not: that one is "which cars does a trip
// ADD", this one is "which of my cars have a window open".
func (r *TripRepo) ActiveTripIDsForUser(ctx context.Context, userID string) (map[string]string, error) {
	const op = "trip.active_ids"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	rows, err := r.pool.Query(ctx, queryActiveTripIDsForUser, userID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.ActiveTripIDsForUser(%s): %w", userID, err)
	}
	defer rows.Close()

	out := make(map[string]string, 2)
	for rows.Next() {
		var vehicleID, tripID string
		if err := rows.Scan(&vehicleID, &tripID); err != nil {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.ActiveTripIDsForUser(%s): scan: %w", userID, err)
		}
		out[vehicleID] = tripID
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.ActiveTripIDsForUser(%s): rows: %w", userID, err)
	}
	return out, nil
}

// RegisterActivityStartToken stores one party's ActivityKit PUSH-TO-START token
// for one trip.
//
// UPSERT, because ActivityKit ROTATES the value: a re-registration must replace
// it in place rather than accumulate rows that would each try to start their
// own Activity on the same phone for the same leg.
//
// P1 CAPABILITY. Whoever holds this token together with the team's APNs signing
// key can start a Live Activity on that phone. It is never logged beyond an
// 8-character prefix, never echoed into a response, and — note the error
// wrapping below — never placed in an error message either, which is the one
// place a value most reliably reaches a log without anybody deciding it should.
func (r *TripRepo) RegisterActivityStartToken(ctx context.Context, tripID, userID, token string, sandbox bool) error {
	const op = "trip.register_activity_token"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	if _, err := r.pool.Exec(ctx, queryUpsertTripActivityToken, tripID, userID, token, sandbox); err != nil {
		r.metrics.IncQueryError(op)
		return fmt.Errorf("TripRepo.RegisterActivityStartToken(trip=%s): %w", tripID, err)
	}
	return nil
}

// DeleteActivityStartToken removes it. Idempotent — no row is the same answer
// as one row removed, and the endpoint answers 204 either way.
func (r *TripRepo) DeleteActivityStartToken(ctx context.Context, tripID, userID string) error {
	const op = "trip.delete_activity_token"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	if _, err := r.pool.Exec(ctx, queryDeleteTripActivityToken, tripID, userID); err != nil {
		r.metrics.IncQueryError(op)
		return fmt.Errorf("TripRepo.DeleteActivityStartToken(trip=%s): %w", tripID, err)
	}
	return nil
}
