package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RESUMING A LEG THAT SHOULD NEVER HAVE CLOSED (MYR-612).
//
// Tesla streams DELTAS, and on 2026-09-08 a car four minutes into a leg to a
// hotel in Sedona sent a frame whose destination name was present-but-EMPTY
// while its `minutesToArrival` still read 98 and the dash still showed the
// place. The detector read it as a cancelled route, closed leg A at 03:40:22
// and opened leg B for the same journey two seconds later.
//
// The primary fix is a DEBOUNCE in internal/trips: an empty name while the car
// is still driving and still reporting an estimate no longer closes anything.
// THIS FILE IS THE SECOND LINE, for the closes debouncing cannot prevent —
// a process restart between the two frames, two servers during a rolling
// deploy, a grace that expired one frame before the name came back. The journey
// stays ONE leg: one banner, one card, one row in the trip's history.
//
// Split from trip_leg_repo.go under the 300-line cap, along a real seam: that
// file owns a leg's ordinary lifecycle, this one owns the repair.

// queryRecentClosedLegForVehicle finds the leg this car most recently closed
// WITHOUT ARRIVING, if it closed recently enough to be resumable (MYR-612).
//
// `arrived = false` is a hard predicate rather than a preference: an arrival is
// a real ending that already fired `trip_leg_arrived`, and *"your car arrived"*
// is a sentence that cannot be taken back — resuming such a leg would put the
// car back on its way to a place it has been told to have reached.
//
// Served by idx_go_trip_legs_vehicle_ended (migration 0049). The destination is
// NOT compared here and cannot be: `destination_name_enc` is sealed with a
// random nonce, so two seals of the same name are different bytes. The caller
// decrypts this row and compares in Go.
const queryRecentClosedLegForVehicle = `
SELECT id, trip_id, vehicle_id, destination_name_enc, started_at, ended_at, arrived,
       started_notified_at, arrived_notified_at, activity_started_at, activity_ended_at
FROM go_trip_legs
WHERE vehicle_id = $1 AND ended_at IS NOT NULL AND arrived = false AND ended_at >= $2
ORDER BY ended_at DESC
LIMIT 1`

// queryResumeLeg re-opens a closed leg.
//
// THE THREE COLUMNS IT TOUCHES ARE EXACTLY THE ONES THAT DESCRIBE AN ENDING,
// and the two it leaves alone are the point:
//
//	ended_at             cleared — the leg is under way again.
//	activity_ended_at    cleared — whatever ending was delivered is undone, so
//	                     the leg's eventual real ending can claim and send.
//	activity_started_at  cleared ONLY IF a card was actually ENDED. A card that
//	                     was ended is gone from the lock screen and the resumed
//	                     leg needs a new one, so the push-to-start claim is
//	                     released; a card still running must NOT be started
//	                     twice, so the claim stands.
//	started_notified_at  UNTOUCHED. The `trip_leg_started` banner already went
//	                     out for this journey and this is the same journey —
//	                     the duplicate banner is precisely what MYR-612 is
//	                     about.
//	arrived_notified_at  UNTOUCHED, and cannot matter: `arrived = false` is a
//	                     precondition of being resumable at all.
//
// Guarded on `ended_at IS NOT NULL` so two racers cannot both resume, and so a
// leg that some other path re-opened first is left alone.
const queryResumeLeg = `
UPDATE go_trip_legs
SET ended_at            = NULL,
    activity_started_at = CASE WHEN activity_ended_at IS NOT NULL THEN NULL ELSE activity_started_at END,
    activity_ended_at   = NULL
WHERE id = $1 AND ended_at IS NOT NULL
RETURNING id`

// ResumeRecentLeg re-opens the leg this car just closed, when the car has set
// off again for the SAME place within the merge window (MYR-612).
//
// Reports (leg, true) when it resumed one and (zero, false, nil) — never an
// error — for every ordinary reason not to: no recent close, a different
// destination, an arrival, a racer that got there first. The caller's next move
// on false is StartLeg, so "no" has to be cheap and unexceptional.
//
// WHY A RESUME RATHER THAN A SECOND ROW. The two are the same journey. A second
// row means a second `trip_leg_started` banner, a second push-to-start fan-out,
// a second card, and a trip history that says the car drove to one hotel twice.
// The debounce in internal/trips is what usually prevents the close; this is
// what makes the close survivable when something outside the detector's memory
// caused it — a restart, a rolling deploy, a grace that expired one frame early.
//
// A UNIQUE VIOLATION IS A "NO", not an error: it means another open leg exists
// for this trip, so this car is already under way and there is nothing to
// resume. StartLeg's own ON CONFLICT then returns that leg.
func (r *TripLegRepo) ResumeRecentLeg(
	ctx context.Context, vehicleID, destination string, notBefore time.Time,
) (TripLeg, bool, error) {
	if destination == "" {
		return TripLeg{}, false, nil
	}
	rows, err := r.pool.Query(ctx, queryRecentClosedLegForVehicle, vehicleID, notBefore)
	if err != nil {
		return TripLeg{}, false, fmt.Errorf("store.ResumeRecentLeg(vehicle=%s): %w", vehicleID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return TripLeg{}, false, fmt.Errorf("store.ResumeRecentLeg(vehicle=%s): %w", vehicleID, err)
		}
		return TripLeg{}, false, nil
	}
	candidate, err := r.scanLegRow(rows)
	if err != nil {
		return TripLeg{}, false, fmt.Errorf("store.ResumeRecentLeg(vehicle=%s): %w", vehicleID, err)
	}
	rows.Close()

	// THE COMPARISON IS ON THE PLAINTEXT, in Go, because the column is sealed
	// with a random nonce and two seals of one name are different bytes. A leg
	// whose destination could not be decrypted reads as "" here and is refused
	// — resuming a leg we cannot name would merge two journeys on no evidence.
	if candidate.DestinationName == "" || candidate.DestinationName != destination {
		return TripLeg{}, false, nil
	}

	var id string
	err = r.pool.QueryRow(ctx, queryResumeLeg, candidate.ID).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Somebody else re-opened or re-closed it between the two statements.
		return TripLeg{}, false, nil
	case isUniqueViolation(err):
		// The trip already has an open leg: nothing to resume.
		return TripLeg{}, false, nil
	case err != nil:
		return TripLeg{}, false, fmt.Errorf("store.ResumeRecentLeg(leg=%s): %w", candidate.ID, err)
	}

	leg, err := r.LegByID(ctx, id)
	if err != nil {
		return TripLeg{}, false, fmt.Errorf("store.ResumeRecentLeg(leg=%s): read back: %w", id, err)
	}
	return leg, true, nil
}
