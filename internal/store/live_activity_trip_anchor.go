package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// THE SECOND ANCHOR on go_live_activities (MYR-602, migration 0047).
//
// A Live Activity row is keyed to the thing the Activity is ABOUT. Until now
// that was always a ride, so `ride_request_id` was NOT NULL. A TRIP LEG is the
// second kind of thing, and the two are mutually exclusive — an Activity
// describes one ride OR one leg, never both and never neither — which migration
// 0047 enforces with `CHECK ((ride_request_id IS NOT NULL) <> (trip_leg_id IS
// NOT NULL))`.
//
// WHY THE SAME TABLE AND NOT A SECOND ONE. A separate go_trip_leg_activities
// would have needed its own copy of the ETA ticker, the held-end machinery, the
// token-rotation upsert and the 24-hour reaper — and four copies of the
// alert-on-update-then-end rule is how one of them ends up wrong. MYR-418 is
// the standing proof that this surface has no failure signal at all: an alert
// on an `end` is accepted by APNs and honoured by nothing, so a second
// implementation would look exactly like a working one from the server all the
// way to the logs.
//
// WHAT IS DELIBERATELY NOT SHARED. The ride path's registration statement
// (queryUpsertLiveActivity) is an INSERT … SELECT guarded on the RIDE's status:
// it refuses a registration on a terminal ride, and on an unrescued expired
// reservation. A leg has no status to guard on — it is open or it is closed —
// so the leg registration guards on `ended_at IS NULL` on the LEG itself, which
// is the same question in the leg's own vocabulary. Registering against a
// closed leg is refused for the same reason a terminal ride is: the card has
// been ended for good, and clearing its tombstone would resume an ETA countdown
// to a place the car already left.

// queryUpsertLegActivity registers a per-Activity UPDATE token against a leg.
//
// The guard is the INSERT … SELECT's WHERE: the leg must exist and still be
// open. A miss affects zero rows, which the caller reports as
// ErrLiveActivityRideClosed — the same sentinel the ride path uses, because the
// HTTP layer's answer is identical (409, "end your Activity locally") and
// minting a second error would make the handler branch on which anchor it
// happened to be holding.
//
// `alerted_phase` is seeded at 0 rather than at a ride-status-derived rung. The
// ladder in activity_alert.go is a RIDE ladder — requested, accepted, arrived,
// enroute, completed — and a leg has none of those states. A leg's Activity
// alerts once when it is pushed to start (which the device draws itself) and
// once at the end; there is no ladder to seed a high-water mark on.
//
// ON CONFLICT clears the tombstone exactly as the ride path does, and for the
// same reason: a client that re-registers is telling us it has a live Activity
// again, and leaving a stale tombstone would silently exclude the row from
// every send path with nothing in the logs to explain the frozen card.
const queryUpsertLegActivity = `
INSERT INTO go_live_activities
    (id, trip_leg_id, user_id, activity_push_token, sandbox, alerted_phase, created_at, updated_at)
SELECT $1, l.id, $3, $4, $5, 0, NOW(), NOW()
FROM go_trip_legs l
WHERE l.id = $2 AND l.ended_at IS NULL
ON CONFLICT (trip_leg_id, user_id) DO UPDATE
SET activity_push_token = EXCLUDED.activity_push_token,
    sandbox             = EXCLUDED.sandbox,
    updated_at          = NOW(),
    ended_at            = NULL`

// queryListLegActivities is the fan-out for one leg's update or end push.
//
// It shares progressColumns with the ride reads so there is ONE spelling of the
// anchor projection — but a leg Activity never carries a progress anchor today
// (the trip content-state's `progress` is absent; see push.tripContentState),
// so every column comes back NULL and scanProgress yields the zero anchor. The
// shared projection is kept anyway rather than trimmed to the columns a leg
// actually uses: the day a leg DOES carry a track, the projection is already
// right, and two different column lists over one table is how one of them
// starts disagreeing about what a track means.
const queryListLegActivities = `
SELECT a.trip_leg_id, a.user_id, a.activity_push_token, a.sandbox,
       ` + progressColumns + `
FROM go_live_activities a
WHERE a.trip_leg_id = $1 AND a.ended_at IS NULL`

// queryEndLegActivities tombstones every Activity on one leg, after the final
// `event: "end"` push. Whole-leg rather than per-user because a leg ends for
// everybody at once — the car parked.
const queryEndLegActivities = `
UPDATE go_live_activities
SET ended_at = NOW(), updated_at = NOW()
WHERE trip_leg_id = $1 AND ended_at IS NULL`

// queryEndLegActivity tombstones ONE person's Activity on a leg — the
// client-initiated end, scoped to the caller so nobody can end another's card.
const queryEndLegActivity = `
UPDATE go_live_activities
SET ended_at = NOW(), updated_at = NOW()
WHERE trip_leg_id = $1 AND user_id = $2 AND ended_at IS NULL`

// RegisterLegActivity upserts the per-Activity update token for one party's
// Live Activity on one trip leg, replacing a rotated token in place and
// clearing any previous end-tombstone.
//
// Returns ErrLiveActivityRideClosed when the leg is gone or already closed —
// the same sentinel and the same 409 as the ride path, see queryUpsertLegActivity.
//
// The caller is responsible for having established that userID is the trip's
// owner or a live participant.
func (r *LiveActivityRepo) RegisterLegActivity(ctx context.Context, legID, userID, token string, sandbox bool) error {
	if strings.TrimSpace(legID) == "" {
		return fmt.Errorf("store.RegisterLegActivity: empty leg id")
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("store.RegisterLegActivity(leg=%s): empty user id", legID)
	}
	if strings.TrimSpace(token) == "" {
		// The token is P1: report its absence, never its value.
		return fmt.Errorf("store.RegisterLegActivity(leg=%s, user=%s): empty activity token", legID, userID)
	}

	tag, err := r.pool.Exec(ctx, queryUpsertLegActivity, newProvisionID(), legID, userID, token, sandbox)
	if err != nil {
		return fmt.Errorf("store.RegisterLegActivity(leg=%s, user=%s): %w", legID, userID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store.RegisterLegActivity(leg=%s, user=%s): %w",
			legID, userID, ErrLiveActivityRideClosed)
	}
	return nil
}

// ActivitiesForLeg returns every still-live Activity registered against a leg.
// An empty slice is the ordinary case: a leg whose participants are all on the
// web, or one whose push-to-start went out a second ago and whose devices have
// not registered their update tokens yet.
func (r *LiveActivityRepo) ActivitiesForLeg(ctx context.Context, legID string) ([]LiveActivity, error) {
	if strings.TrimSpace(legID) == "" {
		return nil, fmt.Errorf("store.ActivitiesForLeg: empty leg id")
	}

	rows, err := r.pool.Query(ctx, queryListLegActivities, legID)
	if err != nil {
		return nil, fmt.Errorf("store.ActivitiesForLeg(leg=%s): %w", legID, err)
	}
	defer rows.Close()

	var out []LiveActivity
	for rows.Next() {
		var a LiveActivity
		var leg, source *string
		var baseline, value, reading *float64
		var readingAt *time.Time
		// Scanned inline rather than through a helper shared with
		// ActivitiesForRide: the two statements differ in their FIRST column
		// (the anchor) and in nothing else, so a shared scanner would need the
		// caller to hand it a destination for that column — a seam that buys
		// nothing over twelve lines and would make both reads harder to read.
		if err := rows.Scan(&a.TripLegID, &a.UserID, &a.ActivityPushToken, &a.Sandbox,
			&leg, &source, &baseline, &value, &reading, &readingAt, &a.AlertedPhase); err != nil {
			return nil, fmt.Errorf("store.ActivitiesForLeg(leg=%s): scan: %w", legID, err)
		}
		a.Progress = scanProgress(leg, source, baseline, value, reading, readingAt)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActivitiesForLeg(leg=%s): iterate: %w", legID, err)
	}
	return out, nil
}

// EndActivitiesForLeg tombstones every live Activity on a leg and reports how
// many it closed. Called after the final `event: "end"` push.
func (r *LiveActivityRepo) EndActivitiesForLeg(ctx context.Context, legID string) (int64, error) {
	if strings.TrimSpace(legID) == "" {
		return 0, fmt.Errorf("store.EndActivitiesForLeg: empty leg id")
	}
	tag, err := r.pool.Exec(ctx, queryEndLegActivities, legID)
	if err != nil {
		return 0, fmt.Errorf("store.EndActivitiesForLeg(leg=%s): %w", legID, err)
	}
	return tag.RowsAffected(), nil
}

// EndLegActivity tombstones the caller's own Activity on a leg, reporting
// whether a live row matched. Idempotent, and a miss is not an error — the same
// reasoning as EndActivity: a card somebody else registered must be
// indistinguishable from one that was never registered.
func (r *LiveActivityRepo) EndLegActivity(ctx context.Context, legID, userID string) (bool, error) {
	if strings.TrimSpace(legID) == "" {
		return false, fmt.Errorf("store.EndLegActivity: empty leg id")
	}
	if strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("store.EndLegActivity(leg=%s): empty user id", legID)
	}
	tag, err := r.pool.Exec(ctx, queryEndLegActivity, legID, userID)
	if err != nil {
		return false, fmt.Errorf("store.EndLegActivity(leg=%s, user=%s): %w", legID, userID, err)
	}
	return tag.RowsAffected() > 0, nil
}
