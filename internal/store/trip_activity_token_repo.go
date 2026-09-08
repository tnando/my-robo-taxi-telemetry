package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// go_trip_activity_tokens (migration 0047, MYR-602): ActivityKit PUSH-TO-START
// tokens, one per (trip, user).
//
// THIS IS NOT go_live_activities AND MUST NOT BE CONFUSED WITH IT. The two hold
// different kinds of token and the difference decides what a 410 means:
//
//	go_trip_activity_tokens  a PUSH-TO-START token. It addresses an APP on a
//	                         phone and says "you may CREATE a Live Activity of
//	                         this type here". It is minted once when the user
//	                         opens the trip, it survives every leg of the trip,
//	                         and there is no Activity behind it.
//	go_live_activities       a per-ACTIVITY UPDATE token. It addresses ONE
//	                         RUNNING CARD and dies with it.
//
// So a 410 on a push-to-start token means THE APP is gone (deleted, or the user
// revoked Live Activities for it) — the row here must go. A 410 on an update
// token means THE CARD is gone and the app is fine. The existing dropActivity
// path deletes from go_live_activities BY TOKEN, and pointing it at a
// push-to-start rejection would delete nothing at all while leaving a dead
// token to be re-tried on every leg for the length of the trip.
//
// P1 CAPABILITY. Whoever holds one of these together with the team's APNs
// signing key can START a Live Activity on that phone. Never logged beyond
// push.TokenPrefix's 8 characters, never echoed into a response, never in an
// error string — the methods below report the ABSENCE of a token, never its
// value.
//
// NOT ENCRYPTED AT REST, for the reason data-classification.md §3.2 gives about
// its sibling go_live_activities.activity_push_token: the sender needs the exact
// bytes on every push, and a round trip through the encryptor buys nothing
// against an attacker who also holds the signing key.

// The TripActivityToken shape, the (trip_id, user_id) upsert and the
// caller-scoped delete live in trip_types.go and trip_queries.go, beside the
// §7.30 endpoints that write them. Only the 410 path is declared here, because
// only this file has a reason to delete a row Apple rejected.
//
// UNGUARDED UPSERT, unlike go_live_activities', and the asymmetry is the point.
// That statement refuses a registration on a ride that has reached a terminal
// status, because an update token addresses a card that must not be
// resurrected. A push-to-start token addresses no card at all — it is a
// standing permission slip for the next leg — so there is nothing to
// resurrect, and refusing it mid-trip would silently stop the trip's remaining
// legs from ever raising an Activity.

// queryDeleteTripActivityTokenByValue removes a token APNs has permanently
// rejected, whoever registered it. NOT caller-scoped, for the same reason as
// the device-registry twin: Apple's verdict is about the installation, not
// about the person whose row happens to name it.
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal (gosec greps the identifier 'token' in a string).
const queryDeleteTripActivityTokenByValue = `
DELETE FROM go_trip_activity_tokens
WHERE push_to_start_token = $1`

// queryTripActivityTokens lists a trip's registrations — the push-to-start
// fan-out for one leg.
//
// IT RE-JOINS THE MEMBERSHIP AND THE SHARE, and that is an access predicate
// rather than tidiness. A registration is a standing CAPABILITY on a phone: the
// row survives a participant leaving the trip and survives the owner suspending
// their share, because nothing in either path deletes it. Listed unconditionally
// by trip id, the next leg would push a Live Activity naming the car and its
// destination to somebody whose access ended — the precise thing "trip access
// cannot outlive the share" promises cannot happen. So the fan-out asks the
// same question every other trips surface asks, with the same two predicates
// (`left_at IS NULL`, `status = 'accepted' AND suspended_at IS NULL`) that
// queryTripAudience and auth.queryActiveTripParticipation carry.
//
// THE OWNER IS ADMITTED UNCONDITIONALLY. They hold no share on their own car —
// there is no grant to check — and they are on the leg card by explicit product
// decision.
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal (gosec greps the identifier 'token' in a string).
const queryTripActivityTokens = `
SELECT tok.trip_id, tok.user_id, tok.push_to_start_token, tok.sandbox
FROM go_trip_activity_tokens tok
JOIN go_trips t ON t.id = tok.trip_id
WHERE tok.trip_id = $1
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
  )`

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

// TripActivityTokenRepo is the go_trip_activity_tokens repository.
type TripActivityTokenRepo struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewTripActivityTokenRepo builds the repository over the given pool.
func NewTripActivityTokenRepo(pool *pgxpool.Pool, logger *slog.Logger) *TripActivityTokenRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &TripActivityTokenRepo{pool: pool, logger: logger}
}

// DeleteRejectedPushToStartToken removes a token APNs answered 410 to.
//
// THE METHOD THE 410 PATH MUST CALL for a push-to-start rejection — not
// DeleteActivityToken, which targets go_live_activities and would delete
// nothing here while leaving a dead token to be retried on every remaining leg.
// See the file header.
func (r *TripActivityTokenRepo) DeleteRejectedPushToStartToken(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("store.DeleteRejectedPushToStartToken: empty token")
	}
	if _, err := r.pool.Exec(ctx, queryDeleteTripActivityTokenByValue, token); err != nil {
		return fmt.Errorf("store.DeleteRejectedPushToStartToken: %w", err)
	}
	return nil
}

// PushToStartTokensForTrip lists the trip's registrations. An empty result is
// the ordinary case — a trip whose participants are all on the web, or one
// nobody has opened yet — and the leg detector treats it as "no cards to
// raise", never as a failure.
func (r *TripActivityTokenRepo) PushToStartTokensForTrip(ctx context.Context, tripID string) ([]TripActivityToken, error) {
	rows, err := r.pool.Query(ctx, queryTripActivityTokens, tripID)
	if err != nil {
		return nil, fmt.Errorf("store.PushToStartTokensForTrip(trip=%s): %w", tripID, err)
	}
	defer rows.Close()

	var out []TripActivityToken
	for rows.Next() {
		var tok TripActivityToken
		if err := rows.Scan(&tok.TripID, &tok.UserID, &tok.PushToStartToken, &tok.Sandbox); err != nil {
			return nil, fmt.Errorf("store.PushToStartTokensForTrip(trip=%s): scan: %w", tripID, err)
		}
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.PushToStartTokensForTrip(trip=%s): iterate: %w", tripID, err)
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
