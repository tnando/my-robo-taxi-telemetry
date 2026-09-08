package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// THE SEND-PATH GATES over go_trip_activity_tokens: who gets a leg's card, who
// has already had it, and who is getting it instead of a banner.
//
// Split from trip_activity_token_repo.go under the 300-line cap, along a real
// seam: that file owns the registry's LIFECYCLE — what the rows are, why they
// are not go_live_activities, and the 410 that deletes one. This one owns the
// three questions asked of them at a leg edge, all three added by MYR-612 and
// MYR-620, all three carrying the same membership predicate for the same
// reason.

// queryClaimPushToStartForLegAll claims the leg's push-to-start on EVERY
// registered device of the trip, in one statement, and returns what it claimed
// (MYR-612 review).
//
// ⚠ IT REPLACES A LIST-THEN-CLAIM-EACH. The fan-out used to read the trip's
// registrations and then run the per-device claim once per row — which re-ran
// this same membership predicate N more times and DISCARDED the P1 tokens the
// first query had already loaded, because only the claim's own RETURNING is
// safe against a rotation between the two. One statement is both cheaper and
// more honest: the tokens it hands back are the ones it stamped.
//
// The per-device form next door still exists and is still needed — the
// registration catch-up claims for exactly one phone, arriving from an HTTP
// handler with no list in hand.
//
// IT RE-JOINS THE MEMBERSHIP AND THE SHARE, and that is an access predicate
// rather than tidiness. A registration is a standing CAPABILITY on a phone: the
// row survives a participant leaving the trip and survives the owner suspending
// their share, because nothing in either path deletes it. Claimed
// unconditionally by trip id, the next leg would push a Live Activity naming
// the car and its destination to somebody whose access ended — the precise
// thing "trip access cannot outlive the share" promises cannot happen. So the
// fan-out asks the same question every other trips surface asks, with the same
// two predicates (`left_at IS NULL`, `status = 'accepted' AND suspended_at IS
// NULL`) that queryTripAudience and auth.queryActiveTripParticipation carry.
//
// THE OWNER IS ADMITTED UNCONDITIONALLY. They hold no share on their own car —
// there is no grant to check — and they are on the leg card by explicit product
// decision.
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal (gosec greps the identifier 'token' in a string).
const queryClaimPushToStartForLegAll = `
UPDATE go_trip_activity_tokens tok
SET started_leg_id = $2, updated_at = NOW()
FROM go_trips t
WHERE tok.trip_id = $1
  AND t.id = tok.trip_id
  AND tok.started_leg_id IS DISTINCT FROM $2
  AND (
        tok.user_id = t.owner_user_id
     OR EXISTS (
            SELECT 1
            FROM go_trip_participants p
            JOIN go_vehicle_shares s
              ON s.vehicle_id = t.vehicle_id
             AND s.accepted_by_user_id = p.user_id
             AND s.status = 'accepted'
             AND s.suspended_at IS NULL
            WHERE p.trip_id = t.id AND p.user_id = tok.user_id AND p.left_at IS NULL
        )
  )
RETURNING tok.trip_id, tok.user_id, tok.push_to_start_token, tok.sandbox`

// queryClaimPushToStartForLeg claims ONE device's push-to-start for ONE leg
// (MYR-612).
//
// CLAIM BEFORE SEND, one row at a time, and it is the only thing standing
// between the two senders and two Live Activities for one journey on one lock
// screen. Both the leg-open fan-out and the registration catch-up go through
// it; `IS DISTINCT FROM` rather than `<>` because the column is NULL until the
// first claim, and `NULL <> 'leg-x'` is NULL, which is not TRUE and would
// therefore claim nothing at all on the very first send.
//
// It RETURNS the token, so the caller sends exactly the bytes it claimed rather
// than bytes it listed a moment earlier — a rotation between the two would
// otherwise send a dead token and stamp the row as done.
//
// The membership predicate is the SAME one queryTripActivityTokens carries, for
// the same reason: a registration is a standing capability that survives a
// participant leaving and a share being suspended, and neither path deletes the
// row. The catch-up reaches this statement straight from an HTTP handler, so it
// has to re-ask rather than inherit an answer.
//
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal.
const queryClaimPushToStartForLeg = `
UPDATE go_trip_activity_tokens tok
SET started_leg_id = $3, updated_at = NOW()
FROM go_trips t
WHERE tok.trip_id = $1 AND tok.user_id = $2
  AND t.id = tok.trip_id
  AND tok.started_leg_id IS DISTINCT FROM $3
  AND (
        tok.user_id = t.owner_user_id
     OR EXISTS (
            SELECT 1
            FROM go_trip_participants p
            JOIN go_vehicle_shares s
              ON s.vehicle_id = t.vehicle_id
             AND s.accepted_by_user_id = p.user_id
             AND s.status = 'accepted'
             AND s.suspended_at IS NULL
            WHERE p.trip_id = t.id AND p.user_id = tok.user_id AND p.left_at IS NULL
        )
  )
RETURNING tok.push_to_start_token, tok.sandbox`

// queryReleasePushToStartClaim gives a claim back after a send that failed for
// a reason that might not repeat.
//
// WITHOUT IT, CLAIM-BEFORE-SEND WOULD MAKE A TRANSIENT APNs ERROR PERMANENT for
// that device on that leg: the row would read as "already started" for the whole
// leg while no card exists. Guarded on the leg id so a release cannot undo a
// LATER leg's claim that landed in between.
//
// A 410 is NOT released — that verdict deletes the row outright, because the
// app itself is gone.
//
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal.
const queryReleasePushToStartClaim = `
UPDATE go_trip_activity_tokens
SET started_leg_id = NULL
WHERE trip_id = $1 AND user_id = $2 AND started_leg_id = $3`

// ClaimPushToStartForLegAll claims the leg's push-to-start on every registered
// device of the trip and returns exactly what it stamped.
//
// An empty result is the ordinary case — a trip whose participants are all on
// the web, one nobody has opened yet, or a leg whose devices the catch-up
// already served — and the caller treats it as "no cards to raise", never as a
// failure.
func (r *TripActivityTokenRepo) ClaimPushToStartForLegAll(
	ctx context.Context, tripID, legID string,
) ([]TripActivityToken, error) {
	if tripID == "" || legID == "" {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, queryClaimPushToStartForLegAll, tripID, legID)
	if err != nil {
		// The LEG is named, the tokens are not: an error string is the one
		// place a P1 value most reliably reaches a log without anybody
		// deciding it should.
		return nil, fmt.Errorf("store.ClaimPushToStartForLegAll(leg=%s): %w", legID, err)
	}
	defer rows.Close()

	var out []TripActivityToken
	for rows.Next() {
		var tok TripActivityToken
		if err := rows.Scan(&tok.TripID, &tok.UserID, &tok.PushToStartToken, &tok.Sandbox); err != nil {
			return nil, fmt.Errorf("store.ClaimPushToStartForLegAll(leg=%s): scan: %w", legID, err)
		}
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ClaimPushToStartForLegAll(leg=%s): iterate: %w", legID, err)
	}
	return out, nil
}

// ClaimPushToStartForLeg claims one device's push-to-start for one leg and
// returns the token to send.
//
// (token, true) means this caller won the claim and MUST send. (·, false) means
// somebody already did, or the caller is no longer admitted to the trip — both
// are ordinary, and neither is an error: the leg-open fan-out and the
// registration catch-up race by design, and exactly one of them should send.
func (r *TripActivityTokenRepo) ClaimPushToStartForLeg(
	ctx context.Context, tripID, userID, legID string,
) (TripActivityToken, bool, error) {
	if tripID == "" || userID == "" || legID == "" {
		return TripActivityToken{}, false, nil
	}
	tok := TripActivityToken{TripID: tripID, UserID: userID}
	err := r.pool.QueryRow(ctx, queryClaimPushToStartForLeg, tripID, userID, legID).
		Scan(&tok.PushToStartToken, &tok.Sandbox)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return TripActivityToken{}, false, nil
	case err != nil:
		// The LEG is named, the token is not: an error string is the one place
		// a P1 value most reliably reaches a log without anybody deciding it
		// should.
		return TripActivityToken{}, false, fmt.Errorf("store.ClaimPushToStartForLeg(leg=%s): %w", legID, err)
	}
	return tok, true, nil
}

// ReleasePushToStartClaim hands a claim back after a send that failed for a
// reason that might not repeat, so a later attempt can retry it.
func (r *TripActivityTokenRepo) ReleasePushToStartClaim(ctx context.Context, tripID, userID, legID string) error {
	if tripID == "" || userID == "" || legID == "" {
		return nil
	}
	if _, err := r.pool.Exec(ctx, queryReleasePushToStartClaim, tripID, userID, legID); err != nil {
		return fmt.Errorf("store.ReleasePushToStartClaim(leg=%s): %w", legID, err)
	}
	return nil
}

// queryHasPushToStartToken answers "is this person's phone registered to
// receive this trip's leg Live Activities" (MYR-620).
//
// NO MEMBERSHIP PREDICATE, unlike its two siblings above, and the asymmetry is
// deliberate. Those two decide who to SEND to, so they re-ask the access
// question. This one decides whether to SUPPRESS a banner, and a stale
// registration belonging to somebody who has left the trip must not be able to
// silence a notification — but such a person is not in the banner's audience
// either, so the question never reaches here for them. Asking only about the
// ROW keeps the gate cheap and keeps its failure direction honest: the answer
// it gives is exactly "does a token exist", which is exactly what "will a card
// appear" depends on.
//
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal.
const queryHasPushToStartToken = `
SELECT 1 FROM go_trip_activity_tokens WHERE trip_id = $1 AND user_id = $2`

// HasPushToStartToken reports whether this party holds a push-to-start
// registration for this trip.
//
// Absence is the ordinary answer and never an error: most trips have some
// participants on the web, and a phone with Live Activities disabled never
// registers at all — which is the case the leg banner exists for.
func (r *TripActivityTokenRepo) HasPushToStartToken(ctx context.Context, tripID, userID string) (bool, error) {
	if tripID == "" || userID == "" {
		return false, nil
	}
	var one int
	err := r.pool.QueryRow(ctx, queryHasPushToStartToken, tripID, userID).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store.HasPushToStartToken(trip=%s): %w", tripID, err)
	}
}
