package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WHO A TRIP'S NOTIFICATIONS GO TO. Split from trip_live_repo.go so both stay
// inside the 300-line cap, and the seam is the one that matters most in this
// package: everything here is an ACCESS question — who may be told about this
// trip right now — while its neighbour is a scheduling question about which
// windows have moved. The two are read by different people for different
// reasons, and the share predicate below is the only load-bearing security
// clause in either.

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
//
// THE PREDICATE LIVES IN THE FILTER, NOT IN THE WHERE, AND THE DIFFERENCE IS A
// TRIP THAT DISAPPEARS. Written as `WHERE p.user_id IS NULL OR s.id IS NOT
// NULL` it is a predicate on the JOINED ROWS: a trip whose every participant's
// share is suspended or unaccepted produces one row per participant, all of
// them with a NULL `s.id` and a non-NULL `p.user_id`, so every row is
// eliminated and the aggregate has no group to build — `ErrTripNotFound` for a
// trip that plainly exists. The consequences are not cosmetic. The leg detector
// reads the audience on EVERY frame of an open leg and returns on the error, so
// the leg never closes, the card is never ended and the owner loses their
// banner; `settleClaimed` loses the trip_ended fan-out on the same error.
//
// Moving it into array_agg's FILTER keeps the trip row alive with an EMPTY
// participant list, which is the true answer: the trip exists, the owner is on
// it, and nobody currently holds a live grant. The OWNER's pushes — the two leg
// events — then still go out, and the participant-only pushes go to nobody,
// which is what a suspended share is supposed to mean.
const queryTripAudience = `
SELECT t.vehicle_id,
       t.owner_user_id,
       COALESCE(
           array_remove(
               array_agg(p.user_id) FILTER (WHERE p.user_id IS NOT NULL AND s.id IS NOT NULL),
               NULL
           ),
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
GROUP BY t.vehicle_id, t.owner_user_id`

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
