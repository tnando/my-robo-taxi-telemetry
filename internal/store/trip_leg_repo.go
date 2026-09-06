package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// go_trip_legs (migration 0047, MYR-602): one row per driving leg a car takes
// inside a trip window.
//
// A leg is DERIVED from telemetry and is over in an hour, so the table exists
// for exactly two reasons and both are about EXACTLY-ONCE:
//
//  1. `trip_leg_started` and `trip_leg_arrived` must each fire ONCE per leg. The
//     only way to make a push idempotent across a restart, a redeploy, or two
//     arrival signals in the same second is a durable stamp.
//  2. The per-leg Live Activity needs a durable ANCHOR. go_live_activities rows
//     are keyed to the thing the Activity is ABOUT; for a ride that is the
//     ride, and for a trip it has to be the leg, because a trip may run for
//     days and contain a dozen of them.
//
// `destination_name_enc` is P1 — a place a car actually drove to — and is
// sealed with the same AES-256-GCM label encryptor that seals
// Vehicle."destinationName" (MYR-447). It is decrypted only where the leg's
// push copy or its content-state needs it, and never logged.

// The TripLeg shape and its Open() predicate live in trip_types.go, beside the
// §7.30 reads that also project a leg. This file owns every statement that
// WRITES one.

// queryOpenLegForVehicle reads the vehicle's open leg, if any. Served by the
// partial index idx_go_trip_legs_vehicle_open.
const queryOpenLegForVehicle = `
SELECT id, trip_id, vehicle_id, destination_name_enc, started_at, ended_at, arrived,
       started_notified_at, arrived_notified_at, activity_started_at, activity_ended_at
FROM go_trip_legs
WHERE vehicle_id = $1 AND ended_at IS NULL
LIMIT 1`

// queryLegByID reads one leg by id, for the send paths that hold only an id.
const queryLegByID = `
SELECT id, trip_id, vehicle_id, destination_name_enc, started_at, ended_at, arrived,
       started_notified_at, arrived_notified_at, activity_started_at, activity_ended_at
FROM go_trip_legs
WHERE id = $1`

// queryStartLeg opens a leg.
//
// ON CONFLICT DO NOTHING against the PARTIAL UNIQUE INDEX
// idx_go_trip_legs_open_per_trip, which permits at most one open leg per trip.
// That index is the real guard and this clause is how the detector survives
// meeting it: a redelivered drive-start, or two processes during a rolling
// deploy, produce zero rows rather than an error, and the caller reads the
// existing leg instead. Relying on the detector's own care would have meant one
// duplicate leg equals one duplicate Live Activity on every participant's lock
// screen for the same journey.
const queryStartLeg = `
INSERT INTO go_trip_legs
    (id, trip_id, vehicle_id, destination_name_enc, started_at, created_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT DO NOTHING
RETURNING id`

// queryEndLeg closes a leg, recording whether there was arrival EVIDENCE.
//
// Guarded on `ended_at IS NULL` so a second close is a no-op rather than a
// re-stamp: the park signal and the window closing can both land on one leg,
// and whichever arrives first is the true end. `arrived` is only ever raised,
// never lowered — a leg that arrived and was then also caught by the window
// close must keep its verdict, because the `trip_leg_arrived` push has already
// gone out on the strength of it.
const queryEndLeg = `
UPDATE go_trip_legs
SET ended_at = $2,
    arrived  = go_trip_legs.arrived OR $3
WHERE id = $1 AND ended_at IS NULL`

// queryClaimLegStartedPush stamps the leg-start push, reporting whether this
// caller won it. Same claim-before-send discipline as the trip lifecycle
// stamps, and the same reasoning: a missed leg push is recoverable by looking
// at the app, a repeating one is not recoverable at all.
const queryClaimLegStartedPush = `
UPDATE go_trip_legs
SET started_notified_at = NOW()
WHERE id = $1 AND started_notified_at IS NULL
RETURNING id`

// queryClaimLegArrivedPush is the arrival counterpart.
const queryClaimLegArrivedPush = `
UPDATE go_trip_legs
SET arrived_notified_at = NOW()
WHERE id = $1 AND arrived_notified_at IS NULL
RETURNING id`

// queryClaimLegActivityStart stamps the push-to-start fan-out.
const queryClaimLegActivityStart = `
UPDATE go_trip_legs
SET activity_started_at = NOW()
WHERE id = $1 AND activity_started_at IS NULL
RETURNING id`

// queryClaimLegActivityEnd stamps the Activity end fan-out.
const queryClaimLegActivityEnd = `
UPDATE go_trip_legs
SET activity_ended_at = NOW()
WHERE id = $1 AND activity_ended_at IS NULL
RETURNING id`

// queryOpenLegsForTrip lists a trip's still-open legs, which at most is one —
// the partial unique index says so — but is returned as a list because the
// trip-end path must close whatever it finds rather than assume the invariant
// it is cleaning up after.
const queryOpenLegsForTrip = `
SELECT id, trip_id, vehicle_id, destination_name_enc, started_at, ended_at, arrived,
       started_notified_at, arrived_notified_at, activity_started_at, activity_ended_at
FROM go_trip_legs
WHERE trip_id = $1 AND ended_at IS NULL`

// TripLegRepo is the go_trip_legs repository.
type TripLegRepo struct {
	pool      *pgxpool.Pool
	encryptor cryptox.Encryptor
	metrics   Metrics
	logger    *slog.Logger
}

// NewTripLegRepo builds the repository. A nil encryptor is refused at
// construction rather than tolerated: `destination_name_enc` is NOT NULL and P1,
// so a repo that could not seal it would fail every write anyway — failing at
// wiring time says why.
func NewTripLegRepo(pool *pgxpool.Pool, enc cryptox.Encryptor, metrics Metrics, logger *slog.Logger) (*TripLegRepo, error) {
	if enc == nil {
		return nil, fmt.Errorf("store.NewTripLegRepo: nil encryptor; destination_name_enc is P1 and NOT NULL")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = NoopMetrics{}
	}
	return &TripLegRepo{pool: pool, encryptor: enc, metrics: metrics, logger: logger}, nil
}

// StartLeg opens a leg for a trip, returning the leg as stored.
//
// IDEMPOTENT AGAINST THE OPEN-LEG INDEX: if the trip already has an open leg,
// no row is inserted and the EXISTING leg is returned, whatever its
// destination. That is the right answer even when the destination differs — a
// car that re-routes mid-leg has not started a second journey, and ending the
// first leg to start another would put two Live Activities and four pushes on
// one drive.
func (r *TripLegRepo) StartLeg(ctx context.Context, tripID, vehicleID, destination string, startedAt time.Time) (TripLeg, error) {
	enc, err := labelToEncString(destination, r.encryptor)
	if err != nil {
		return TripLeg{}, fmt.Errorf("store.StartLeg(trip=%s): seal destination: %w", tripID, err)
	}
	if enc == "" {
		// A leg is DEFINED as driving with a destination; the detector must not
		// call this without one. Refused rather than stored as NULL, which the
		// NOT NULL column would reject with a less legible error.
		return TripLeg{}, fmt.Errorf("store.StartLeg(trip=%s): empty destination", tripID)
	}

	var id string
	err = r.pool.QueryRow(ctx, queryStartLeg, newProvisionID(), tripID, vehicleID, enc, startedAt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The open-leg index refused: this trip already has one.
		leg, openErr := r.OpenLegForVehicle(ctx, vehicleID)
		if openErr != nil {
			return TripLeg{}, fmt.Errorf("store.StartLeg(trip=%s): read existing open leg: %w", tripID, openErr)
		}
		return leg, nil
	}
	if err != nil {
		return TripLeg{}, fmt.Errorf("store.StartLeg(trip=%s): %w", tripID, err)
	}
	return r.LegByID(ctx, id)
}

// EndLeg closes a leg. `arrived` records whether there was arrival evidence;
// see queryEndLeg for why it can only ever be raised.
func (r *TripLegRepo) EndLeg(ctx context.Context, legID string, endedAt time.Time, arrived bool) error {
	if _, err := r.pool.Exec(ctx, queryEndLeg, legID, endedAt, arrived); err != nil {
		return fmt.Errorf("store.EndLeg(leg=%s): %w", legID, err)
	}
	return nil
}

// OpenLegForVehicle reads the car's open leg. A zero TripLeg with a nil error
// means there is none, which is the ordinary state of every car.
func (r *TripLegRepo) OpenLegForVehicle(ctx context.Context, vehicleID string) (TripLeg, error) {
	return r.scanLeg(ctx, queryOpenLegForVehicle, "OpenLegForVehicle", vehicleID)
}

// LegByID reads one leg.
func (r *TripLegRepo) LegByID(ctx context.Context, legID string) (TripLeg, error) {
	return r.scanLeg(ctx, queryLegByID, "LegByID", legID)
}

// OpenLegsForTrip lists a trip's open legs, for the end-of-trip teardown.
func (r *TripLegRepo) OpenLegsForTrip(ctx context.Context, tripID string) ([]TripLeg, error) {
	rows, err := r.pool.Query(ctx, queryOpenLegsForTrip, tripID)
	if err != nil {
		return nil, fmt.Errorf("store.OpenLegsForTrip(trip=%s): %w", tripID, err)
	}
	defer rows.Close()

	var out []TripLeg
	for rows.Next() {
		leg, scanErr := r.scanLegRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store.OpenLegsForTrip(trip=%s): %w", tripID, scanErr)
		}
		out = append(out, leg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.OpenLegsForTrip(trip=%s): iterate: %w", tripID, err)
	}
	return out, nil
}

// ClaimLegStartedPush stamps the leg-start push and reports whether this caller
// won it.
func (r *TripLegRepo) ClaimLegStartedPush(ctx context.Context, legID string) (bool, error) {
	return r.claim(ctx, queryClaimLegStartedPush, "ClaimLegStartedPush", legID)
}

// ClaimLegArrivedPush stamps the arrival push and reports whether this caller
// won it.
func (r *TripLegRepo) ClaimLegArrivedPush(ctx context.Context, legID string) (bool, error) {
	return r.claim(ctx, queryClaimLegArrivedPush, "ClaimLegArrivedPush", legID)
}

// ClaimLegActivityStart stamps the push-to-start fan-out.
func (r *TripLegRepo) ClaimLegActivityStart(ctx context.Context, legID string) (bool, error) {
	return r.claim(ctx, queryClaimLegActivityStart, "ClaimLegActivityStart", legID)
}

// ClaimLegActivityEnd stamps the Activity end fan-out.
func (r *TripLegRepo) ClaimLegActivityEnd(ctx context.Context, legID string) (bool, error) {
	return r.claim(ctx, queryClaimLegActivityEnd, "ClaimLegActivityEnd", legID)
}

func (r *TripLegRepo) claim(ctx context.Context, query, op, legID string) (bool, error) {
	var id string
	err := r.pool.QueryRow(ctx, query, legID).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store.%s(leg=%s): %w", op, legID, err)
	}
}

func (r *TripLegRepo) scanLeg(ctx context.Context, query, op, arg string) (TripLeg, error) {
	rows, err := r.pool.Query(ctx, query, arg)
	if err != nil {
		return TripLeg{}, fmt.Errorf("store.%s(%s): %w", op, arg, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return TripLeg{}, fmt.Errorf("store.%s(%s): %w", op, arg, err)
		}
		return TripLeg{}, nil
	}
	leg, scanErr := r.scanLegRow(rows)
	if scanErr != nil {
		return TripLeg{}, fmt.Errorf("store.%s(%s): %w", op, arg, scanErr)
	}
	return leg, nil
}

// scanLegRow projects one row, decrypting the destination fail-soft.
func (r *TripLegRepo) scanLegRow(rows pgx.Rows) (TripLeg, error) {
	var leg TripLeg
	var destEnc *string
	if err := rows.Scan(
		&leg.ID, &leg.TripID, &leg.VehicleID, &destEnc, &leg.StartedAt, &leg.EndedAt, &leg.Arrived,
		&leg.StartedNotifiedAt, &leg.ArrivedNotifiedAt, &leg.ActivityStartedAt, &leg.ActivityEndedAt,
	); err != nil {
		return TripLeg{}, fmt.Errorf("scan: %w", err)
	}
	leg.DestinationName = encStringToLabel(destEnc, r.encryptor, r.logger, r.metrics, "destination_name_enc")
	return leg, nil
}
