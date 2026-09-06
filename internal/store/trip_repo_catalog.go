package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
// PARTICIPANT ROWS ONLY. An owner's own cars are the first leg of the catalog
// already; re-emitting them here would produce duplicates the merge discards
// anyway, and the owner's `activeTripId` is resolved per row instead.
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

// ActiveTripID resolves VehicleSummary.activeTripId for ONE (caller, vehicle)
// pair — the owner's own row, and the snapshot.
//
// Empty string means none. The owner arm carries no share join (an owner holds
// no grant); the participant arm carries the full live-grant predicate, so the
// field can never name a trip whose access the caller does not actually have.
func (r *TripRepo) ActiveTripID(ctx context.Context, userID, vehicleID string) (string, error) {
	const op = "trip.active_id"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	var tripID string
	err := r.pool.QueryRow(ctx, queryActiveTripIDForUserVehicle, userID, vehicleID).Scan(&tripID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		r.metrics.IncQueryError(op)
		return "", fmt.Errorf("TripRepo.ActiveTripID(vehicle=%s): %w", vehicleID, err)
	}
	return tripID, nil
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
