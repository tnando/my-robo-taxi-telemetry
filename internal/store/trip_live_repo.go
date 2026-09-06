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

// The LIVE half of trips (MYR-602): the reads and writes the trip sweeper, the
// leg detector and the Live Activity sender need, and nothing the REST surface
// needs.
//
// It is a SEPARATE repository from the trips CRUD repo on purpose, and the
// split is by CALLER rather than by table. Everything here runs on a timer or
// on a telemetry frame, with no request behind it, no caller to answer and
// nothing to authorize — the authorization already happened when the owner
// created the trip and chose its participants. Statements written for that
// world look different: they claim work atomically instead of reading and then
// writing, they are idempotent by construction because nothing retries them on
// purpose, and every one of them is scoped by a WINDOW rather than by a user.
//
// P1 discipline: `name_enc` is never read here (the sweeper's pushes name the
// CAR, never the trip), and `destination_name_enc` is decrypted only where a
// leg's copy or content-state needs it.

// queryClaimTripsToStart atomically claims the trips whose window has OPENED
// and whose `trip_started` push has not gone out.
//
// CLAIM-AND-RETURN in one statement, not read-then-write. Two processes during
// a rolling deploy would otherwise both read the same unstamped trip and both
// fan out, and "at most once" is the only promise a lifecycle push can keep —
// there is no later correction for a notification somebody already read. The
// UPDATE's own row lock arbitrates; the loser's UPDATE matches zero rows.
//
// THE STAMP IS WRITTEN BEFORE THE PUSH, which is the opposite of the ordering
// the Live Activity marks use, and the reason is that the two failure modes are
// not symmetric here. Stamping first means a process that dies mid-fan-out
// loses that trip's push; stamping after would mean a process that dies after
// APNs accepted re-sends it on the next pass, to everybody, forever, until it
// happens to survive. A missed "your trip started" is recoverable by opening
// the app — the trip is right there — and a repeating one is not recoverable at
// all.
//
// `ended_at IS NULL OR ends_at` is not consulted for the start claim beyond the
// window itself: a trip created with a window already open (the stated
// retroactive case) starts immediately, and one the owner ended before it began
// is excluded by the effective-end predicate.
const queryClaimTripsToStart = `
UPDATE go_trips
SET started_notified_at = NOW(), updated_at = NOW()
WHERE id IN (
    SELECT id FROM go_trips
    WHERE started_notified_at IS NULL
      AND starts_at <= NOW()
      AND NOW() < LEAST(ends_at, COALESCE(ended_at, ends_at))
    ORDER BY starts_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING id`

// queryClaimTripsToEnd is the mirror for the closing edge.
//
// The effective end is LEAST(ends_at, ended_at) — the owner's early end wins
// over the scheduled one — computed here rather than written back over
// `ends_at`, for the reason migration 0047 states: overwriting would destroy
// the owner's stated intent and make an accidental early end unexplainable.
//
// A trip that was never STARTED can still end: one whose whole window elapsed
// while the server was down, or one the owner ended before it opened. Its
// `trip_started` push is simply never sent, which is right — announcing the
// start of a trip that is already over would be worse than silence.
const queryClaimTripsToEnd = `
UPDATE go_trips
SET ended_notified_at = NOW(), updated_at = NOW()
WHERE id IN (
    SELECT id FROM go_trips
    WHERE ended_notified_at IS NULL
      AND LEAST(ends_at, COALESCE(ended_at, ends_at)) <= NOW()
    ORDER BY ends_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING id`

// querySettleTripNow claims the END transition for ONE named trip, whatever the
// clock says — the seam the owner's early-end handler calls so the participants
// hear about it in the same second rather than up to a sweep later.
//
// It is the same claim as queryClaimTripsToEnd, narrowed to one id and with the
// window predicate dropped: the caller has already written `ended_at`, and
// re-deriving the effective end here would just re-read what they wrote. The
// stamp still arbitrates, so an early end racing the sweeper's own pass
// produces exactly one fan-out.
const querySettleTripNow = `
UPDATE go_trips
SET ended_notified_at = NOW(), updated_at = NOW()
WHERE id = $1 AND ended_notified_at IS NULL
RETURNING id`

// querySettleTripStartNow claims the START transition for ONE named trip whose
// window is open — the seam the CREATE handler calls when `startsAt` is already
// in the past, which is how the legs of a road trip already driven join a trip
// retroactively.
//
// The window predicate is KEPT here, unlike its end-side sibling: an end is
// claimed on the strength of a column the caller just wrote, whereas a start
// has no such write behind it — the trip simply is or is not open — and
// announcing the start of a trip that has not begun would be the one direction
// this stamp can never take back.
const querySettleTripStartNow = `
UPDATE go_trips
SET started_notified_at = NOW(), updated_at = NOW()
WHERE id = $1
  AND started_notified_at IS NULL
  AND starts_at <= NOW()
  AND NOW() < LEAST(ends_at, COALESCE(ended_at, ends_at))
RETURNING id`

// queryTripName reads a trip's sealed name, for the Live Activity card.
const queryTripName = `SELECT name_enc FROM go_trips WHERE id = $1`

// TripLiveRepo is the timer-and-telemetry-side repository for trips.
type TripLiveRepo struct {
	pool      *pgxpool.Pool
	encryptor cryptox.Encryptor
	metrics   Metrics
	logger    *slog.Logger
}

// NewTripLiveRepo builds the repository. The encryptor is required for the same
// reason TripLegRepo's is: `name_enc` is NOT NULL and P1, so a repo that could
// not open it would return an empty name on every card rather than say why.
func NewTripLiveRepo(pool *pgxpool.Pool, enc cryptox.Encryptor, metrics Metrics, logger *slog.Logger) (*TripLiveRepo, error) {
	if enc == nil {
		return nil, fmt.Errorf("store.NewTripLiveRepo: nil encryptor; go_trips.name_enc is P1 and NOT NULL")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = NoopMetrics{}
	}
	return &TripLiveRepo{pool: pool, encryptor: enc, metrics: metrics, logger: logger}, nil
}

// ClaimTripsToStart stamps and returns the trips whose window just opened.
func (r *TripLiveRepo) ClaimTripsToStart(ctx context.Context, limit int) ([]string, error) {
	return r.claimTrips(ctx, queryClaimTripsToStart, "ClaimTripsToStart", limit)
}

// ClaimTripsToEnd stamps and returns the trips whose effective end has passed.
func (r *TripLiveRepo) ClaimTripsToEnd(ctx context.Context, limit int) ([]string, error) {
	return r.claimTrips(ctx, queryClaimTripsToEnd, "ClaimTripsToEnd", limit)
}

// claimTrips runs one of the two claim statements.
func (r *TripLiveRepo) claimTrips(ctx context.Context, query, op string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store.%s: non-positive limit %d", op, limit)
	}
	rows, err := r.pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("store.%s(limit=%d): %w", op, limit, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.%s: scan: %w", op, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.%s: iterate: %w", op, err)
	}
	return ids, nil
}

// ClaimTripEndNow claims the end transition for one trip regardless of the
// clock, reporting whether THIS caller won it. False means somebody already
// did — the sweeper, or a second tap on End trip — and is not an error.
func (r *TripLiveRepo) ClaimTripEndNow(ctx context.Context, tripID string) (bool, error) {
	var claimed string
	err := r.pool.QueryRow(ctx, querySettleTripNow, tripID).Scan(&claimed)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store.ClaimTripEndNow(trip=%s): %w", tripID, err)
	}
}

// ClaimTripStartNow claims the start transition for one trip whose window is
// already open, reporting whether THIS caller won it. False means somebody
// already did — the sweeper's own pass — and is not an error.
func (r *TripLiveRepo) ClaimTripStartNow(ctx context.Context, tripID string) (bool, error) {
	var claimed string
	err := r.pool.QueryRow(ctx, querySettleTripStartNow, tripID).Scan(&claimed)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store.ClaimTripStartNow(trip=%s): %w", tripID, err)
	}
}

// TripNameFor decrypts one trip's name.
//
// P1 USER CONTENT. The only consumer is the Live Activity's content-state,
// which is addressed by a token scoped to one card on one device; it must never
// reach an alert body, a push title or a log line. Fail-soft on a decrypt
// failure, like every other label read in this package: "" routes the card to a
// name-less rendering rather than failing the whole push.
func (r *TripLiveRepo) TripNameFor(ctx context.Context, tripID string) (string, error) {
	var nameEnc *string
	err := r.pool.QueryRow(ctx, queryTripName, tripID).Scan(&nameEnc)
	switch {
	case err == nil:
		return encStringToLabel(nameEnc, r.encryptor, r.logger, r.metrics, "name_enc"), nil
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("store.TripNameFor(trip=%s): %w", tripID, ErrTripNotFound)
	default:
		return "", fmt.Errorf("store.TripNameFor(trip=%s): %w", tripID, err)
	}
}

// tripSweepTimeout bounds one claim or one audience read. Generous for an
// indexed statement, short enough that a database stall cannot hold a 60-second
// sweep past its own interval.
const tripSweepTimeout = 5 * time.Second

// SweepTimeout exposes the bound so the sweeper and its tests agree on it
// without either owning the number.
func SweepTimeout() time.Duration { return tripSweepTimeout }
