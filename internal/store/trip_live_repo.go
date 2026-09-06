package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// TripAudience is who a trips notification goes to, and what it is about.
//
// The owner is carried SEPARATELY from the participants rather than merged into
// one list, because the two audiences differ per event: the three lifecycle
// pushes go to participants only (the owner performed the action, or scheduled
// it), while the two LEG pushes go to the owner as well — they are included in
// the per-leg Live Activity by explicit product decision, and a card with no
// banner behind it would be the only surface in the feature that is silent for
// the person driving.
type TripAudience struct {
	TripID      string
	VehicleID   string
	OwnerUserID string
	// ParticipantUserIDs are the LIVE participants: not departed, and still
	// holding an accepted, unsuspended share. The share join is what makes
	// "trip access can never outlive the share" structural here too — a
	// notification is access, in the small.
	ParticipantUserIDs []string
}

// queryTripAudience resolves one trip's recipients.
//
// The participant list is aggregated in SQL rather than fetched as rows,
// because every caller wants exactly the slice and a second round trip per trip
// on a 60-second sweep is a cost with no reader. array_remove strips the NULL
// that array_agg produces for a trip whose participants have all left.
//
// THE SHARE JOIN IS AN ACCESS PREDICATE, not a filter for tidiness, and it is
// the same pair (`status = 'accepted' AND suspended_at IS NULL`) that
// auth.queryUserVehicleIDs and auth.queryActiveTripParticipation carry. A
// suspended grantee must be indistinguishable from no grantee on EVERY surface,
// and a push naming somebody's car is a surface.
const queryTripAudience = `
SELECT t.vehicle_id,
       t.owner_user_id,
       COALESCE(
           array_remove(array_agg(p.user_id) FILTER (WHERE p.user_id IS NOT NULL), NULL),
           '{}'
       )
FROM go_trips t
LEFT JOIN go_trip_participants p
       ON p.trip_id = t.id AND p.left_at IS NULL
LEFT JOIN go_vehicle_shares s
       ON s.vehicle_id = t.vehicle_id
      AND s.accepted_by_user_id = p.user_id
      AND s.status = 'accepted'
      AND s.suspended_at IS NULL
WHERE t.id = $1
  AND (p.user_id IS NULL OR s.id IS NOT NULL)
GROUP BY t.vehicle_id, t.owner_user_id`

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

// queryActiveTripForVehicle answers the leg detector's per-frame question:
// does this car have an OPEN trip window right now, and if so which?
//
// It is deliberately NOT keyed on a user — a leg belongs to the trip, not to a
// viewer — and it returns at most one row because the create endpoint refuses
// an overlapping window on the same vehicle (409 trip_overlaps). `LIMIT 1` is
// the belt to that braces: two overlapping trips from a pre-guard row would
// produce one leg on the older one rather than two Live Activities per journey.
const queryActiveTripForVehicle = `
SELECT id
FROM go_trips
WHERE vehicle_id = $1
  AND starts_at <= NOW()
  AND NOW() < LEAST(ends_at, COALESCE(ended_at, ends_at))
ORDER BY starts_at
LIMIT 1`

// queryActiveTripVehicles lists every vehicle with an open window, for the leg
// detector's candidate cache. Bounded, and DISTINCT because the same car cannot
// legitimately hold two open windows but must not produce two cache entries if
// it somehow does.
const queryActiveTripVehicles = `
SELECT DISTINCT ON (vehicle_id) vehicle_id, id
FROM go_trips
WHERE starts_at <= NOW()
  AND NOW() < LEAST(ends_at, COALESCE(ended_at, ends_at))
ORDER BY vehicle_id, starts_at
LIMIT $1`

// TripLiveRepo is the timer-and-telemetry-side repository for trips.
type TripLiveRepo struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewTripLiveRepo builds the repository over the given pool.
func NewTripLiveRepo(pool *pgxpool.Pool, logger *slog.Logger) *TripLiveRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &TripLiveRepo{pool: pool, logger: logger}
}

// ErrTripNotFound reports that no trip row matched. Returned by TripAudienceFor
// for a trip that was deleted underneath a sweep, which races every pass and is
// an ordinary outcome rather than a failure.
var ErrTripNotFound = errors.New("store: trip not found")

// TripAudienceFor resolves one trip's push recipients.
func (r *TripLiveRepo) TripAudienceFor(ctx context.Context, tripID string) (TripAudience, error) {
	out := TripAudience{TripID: tripID}
	err := r.pool.QueryRow(ctx, queryTripAudience, tripID).
		Scan(&out.VehicleID, &out.OwnerUserID, &out.ParticipantUserIDs)
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, pgx.ErrNoRows):
		return TripAudience{}, fmt.Errorf("store.TripAudienceFor(trip=%s): %w", tripID, ErrTripNotFound)
	default:
		return TripAudience{}, fmt.Errorf("store.TripAudienceFor(trip=%s): %w", tripID, err)
	}
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

// ActiveTripForVehicle returns the id of the vehicle's open trip window, or ""
// when there is none. An absent window is the ordinary answer for most cars.
func (r *TripLiveRepo) ActiveTripForVehicle(ctx context.Context, vehicleID string) (string, error) {
	var tripID string
	err := r.pool.QueryRow(ctx, queryActiveTripForVehicle, vehicleID).Scan(&tripID)
	switch {
	case err == nil:
		return tripID, nil
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	default:
		return "", fmt.Errorf("store.ActiveTripForVehicle(vehicle=%s): %w", vehicleID, err)
	}
}

// TripVehicle pairs a car with the open trip window it is inside.
type TripVehicle struct {
	VehicleID string
	TripID    string
}

// ActiveTripVehicles lists the cars with an open window, capped at limit. The
// leg detector caches this rather than asking per frame.
func (r *TripLiveRepo) ActiveTripVehicles(ctx context.Context, limit int) ([]TripVehicle, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store.ActiveTripVehicles: non-positive limit %d", limit)
	}
	rows, err := r.pool.Query(ctx, queryActiveTripVehicles, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveTripVehicles(limit=%d): %w", limit, err)
	}
	defer rows.Close()

	var out []TripVehicle
	for rows.Next() {
		var tv TripVehicle
		if err := rows.Scan(&tv.VehicleID, &tv.TripID); err != nil {
			return nil, fmt.Errorf("store.ActiveTripVehicles: scan: %w", err)
		}
		out = append(out, tv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActiveTripVehicles: iterate: %w", err)
	}
	return out, nil
}

// tripSweepTimeout bounds one claim or one audience read. Generous for an
// indexed statement, short enough that a database stall cannot hold a 60-second
// sweep past its own interval.
const tripSweepTimeout = 5 * time.Second

// SweepTimeout exposes the bound so the sweeper and its tests agree on it
// without either owning the number.
func SweepTimeout() time.Duration { return tripSweepTimeout }
