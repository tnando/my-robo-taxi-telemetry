package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// TripRepo is the MYR-602 trips aggregate: go_trips and its three children
// (participants, push-to-start tokens, legs), plus the window-scoped reads the
// drives and catalog surfaces resolve through.
//
// It OWNS the window and READS the legs. The leg rows are written by the leg
// detector; nothing in this file inserts, updates or ends one — the only leg
// statement here is queryTripOpenLeg, which the trip card reads.
type TripRepo struct {
	pool    *pgxpool.Pool
	metrics Metrics

	// encryptor seals the trip name and opens the leg destination. MANDATORY,
	// not optional: `name_enc` and `destination_name_enc` are NOT NULL columns
	// with no plaintext sibling, so an unconfigured encryptor would not degrade
	// the feature — it would make every write fail at the constraint and every
	// read return a ciphertext to the user. The DriveRepo's nil-tolerant shape
	// exists because its labels are DUAL-written beside plaintext columns;
	// there is no such fallback here, and pretending otherwise would ship a
	// broken state instead of refusing to boot.
	encryptor cryptox.Encryptor

	logger *slog.Logger
}

// NewTripRepo constructs the repository. Panics on a nil encryptor, matching
// NewRideRequestRepo and NewSavedPlacesRepo: a composition-root mistake that
// would silently disable encryption on P1 user content must stop the process at
// boot, not surface as a failed write during somebody's road trip.
func NewTripRepo(pool *pgxpool.Pool, metrics Metrics, encryptor cryptox.Encryptor, logger *slog.Logger) *TripRepo {
	if encryptor == nil {
		panic("store.NewTripRepo: encryptor must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TripRepo{pool: pool, metrics: metrics, encryptor: encryptor, logger: logger}
}

// tripIDRandomBytes matches the ride id's entropy. Ids are minted here rather
// than by the database for the same reason go_ride_requests mints its own: the
// shape must be a cuid so the platform's ids are indistinguishable from the
// Prisma-side ones no client can tell apart today.
const tripIDRandomBytes = 16

// tripQuerier is the subset of pgx both *pgxpool.Pool and pgx.Tx satisfy, so
// every statement in this aggregate can run either inside the create/patch
// transaction or standalone without a second copy.
//
// Declared here rather than reusing the package's pgxQuerier because that one
// carries only QueryRow, and half of these statements return sets.
type tripQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func newTripID() string { return "c" + randomHex(tripIDRandomBytes) }

// Create opens a window, admits its participants, and returns the trip as its
// owner sees it.
//
// ONE TRANSACTION FOR THE WHOLE THING, because a trip that exists with an empty
// roster is worse than no trip: the owner sees a live window they believe they
// shared and nobody has access to it. All four steps commit together or none
// of them do.
//
// THE VEHICLE IS ADVISORY-LOCKED FIRST, and that is what makes the overlap rule
// actually hold. The probe is a read and the insert is a write, so two
// concurrent creates on one car would both find no overlap and both commit —
// producing exactly the double window the 409 exists to prevent. A transaction
// advisory lock keyed on the vehicle serialises them for the length of the
// transaction and is released by the commit or the rollback with nothing to
// clean up. It does not touch any other vehicle.
func (r *TripRepo) Create(ctx context.Context, in CreateTripInput) (TripView, error) {
	const op = "trip.create"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	if err := validateTripWindow(in.StartsAt, in.EndsAt); err != nil {
		return TripView{}, err
	}
	nameEnc, err := labelToEncString(in.Name, r.encryptor)
	if err != nil {
		// The plaintext is NOT in the error. It is P1 user content and an
		// error string is the one place a value reliably reaches a log.
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): seal name: %w", in.VehicleID, err)
	}

	tripID := newTripID()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): begin: %w", in.VehicleID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockVehicleTrips(ctx, tx, in.VehicleID); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): %w", in.VehicleID, err)
	}

	overlaps, err := tripWindowOverlaps(ctx, tx, in.VehicleID, in.StartsAt, in.EndsAt, "")
	if err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): %w", in.VehicleID, err)
	}
	if overlaps {
		return TripView{}, ErrTripOverlap
	}

	participants, err := resolveShareParticipants(ctx, tx, in.VehicleID, in.ParticipantShareIDs)
	if err != nil {
		if errors.Is(err, ErrTripParticipantNotShared) {
			return TripView{}, err
		}
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): %w", in.VehicleID, err)
	}

	if _, err := tx.Exec(ctx, queryInsertTrip,
		tripID, in.VehicleID, in.OwnerUserID, nameEnc, in.StartsAt, in.EndsAt,
	); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): insert: %w", in.VehicleID, err)
	}

	if err := addTripParticipants(ctx, tx, tripID, participants); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): %w", in.VehicleID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Create(vehicle=%s): commit: %w", in.VehicleID, err)
	}

	// Read back through the ONE view builder rather than assembling a second
	// shape here, so a create response and a subsequent GET cannot disagree
	// about the trip that was just made.
	return r.GetForUser(ctx, tripID, in.OwnerUserID)
}

// lockVehicleTrips takes a transaction-scoped advisory lock on one vehicle's
// trip calendar. Released by COMMIT or ROLLBACK — there is nothing to unlock
// and no way to leak one.
//
// hashtext over the cuid rather than a real id: advisory locks are keyed by
// integer, the key space is a namespace of our choosing, and a collision
// between two vehicles costs a moment of serialisation on an operation that
// happens a handful of times a day.
func lockVehicleTrips(ctx context.Context, tx pgx.Tx, vehicleID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "go_trips:"+vehicleID); err != nil {
		return fmt.Errorf("lock vehicle trips: %w", err)
	}
	return nil
}

// tripWindowOverlaps runs the 409 probe. excludeTripID is the empty string on
// create and the trip's own id on patch, so extending a window does not collide
// with itself.
func tripWindowOverlaps(ctx context.Context, q tripQuerier, vehicleID string, startsAt, endsAt time.Time, excludeTripID string) (bool, error) {
	var overlaps bool
	if err := q.QueryRow(ctx, queryTripOverlaps, vehicleID, startsAt, endsAt, excludeTripID).Scan(&overlaps); err != nil {
		return false, fmt.Errorf("overlap probe: %w", err)
	}
	return overlaps, nil
}

// resolveShareParticipants turns share ids into (shareID, userID) pairs,
// refusing the whole set unless EVERY id is a live accepted grant on this
// vehicle.
//
// ALL OR NOTHING, and the comparison is on COUNTS rather than on which id fell
// out. "No such share", "a share on someone else's car", "an invite never
// redeemed" and "a suspended grant" are one answer, because reporting which
// would make the endpoint an oracle for other people's share ids.
//
// Duplicate ids in the request collapse (the statement returns each row once),
// so a client that sends the same person twice gets one participant rather than
// an error about a count mismatch it cannot diagnose.
func resolveShareParticipants(ctx context.Context, q tripQuerier, vehicleID string, shareIDs []string) ([]TripParticipantView, error) {
	unique := dedupeStrings(shareIDs)
	if len(unique) == 0 {
		return nil, nil
	}

	rows, err := q.Query(ctx, queryAcceptedShareParticipants, vehicleID, unique)
	if err != nil {
		return nil, fmt.Errorf("resolve participants: %w", err)
	}
	defer rows.Close()

	out := make([]TripParticipantView, 0, len(unique))
	for rows.Next() {
		var p TripParticipantView
		if err := rows.Scan(&p.ParticipantID, &p.UserID); err != nil {
			return nil, fmt.Errorf("resolve participants: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve participants: rows: %w", err)
	}
	if len(out) != len(unique) {
		return nil, ErrTripParticipantNotShared
	}
	return out, nil
}

// addTripParticipants writes (or revives) one membership per resolved person.
func addTripParticipants(ctx context.Context, q tripQuerier, tripID string, participants []TripParticipantView) error {
	for _, p := range participants {
		if _, err := q.Exec(ctx, queryUpsertTripParticipant, tripID, p.UserID, p.ParticipantID); err != nil {
			return fmt.Errorf("add participant: %w", err)
		}
	}
	return nil
}

// dedupeStrings returns the input with empties and repeats removed, order
// preserved.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
