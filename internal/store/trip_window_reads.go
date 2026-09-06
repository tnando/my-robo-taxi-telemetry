package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WHICH CARS ARE INSIDE AN OPEN WINDOW RIGHT NOW — the leg detector's two
// reads, in bulk and for one car.
//
// NOT NAMED `trip_open_windows.go`: Go reads a trailing `_windows` as the GOOS
// build constraint, so that filename would have compiled the whole file out of
// every non-Windows build — silently, as an ignored file rather than an error.
//
// Split from trip_live_repo.go so both stay inside the 300-line cap, and along
// a real seam: the claims next door MUTATE (they stamp a boundary event exactly
// once), while both statements here are pure reads asking the same question at
// two granularities. They carry the SAME window predicate character for
// character, which is the property that stops the bulk snapshot and the
// per-write confirmation from ever disagreeing about one car.

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

// ActiveTripVehicle pairs a car with the open trip window it is inside. Named
// apart from TripVehicle (trip_view.go), which is the CATALOG subset a trip
// read projects — this one is the leg detector's candidate row and carries
// nothing but the two ids.
type ActiveTripVehicle struct {
	VehicleID string
	TripID    string
}

// ActiveTripVehicles lists the cars with an open window, capped at limit. The
// leg detector caches this rather than asking per frame.
func (r *TripLiveRepo) ActiveTripVehicles(ctx context.Context, limit int) ([]ActiveTripVehicle, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store.ActiveTripVehicles: non-positive limit %d", limit)
	}
	rows, err := r.pool.Query(ctx, queryActiveTripVehicles, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveTripVehicles(limit=%d): %w", limit, err)
	}
	defer rows.Close()

	var out []ActiveTripVehicle
	for rows.Next() {
		var tv ActiveTripVehicle
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
