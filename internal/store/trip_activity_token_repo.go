package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal (gosec greps the identifier 'token' in a string).
const queryTripActivityTokens = `
SELECT trip_id, user_id, push_to_start_token, sandbox
FROM go_trip_activity_tokens
WHERE trip_id = $1`

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

// RegisterPushToStartToken upserts one person's token for one trip.
//
// The caller is responsible for having established that userID is the trip's
// owner or a live participant — this method enforces the shape of the write,
// not the authorization behind it, exactly as RegisterActivity does.
func (r *TripActivityTokenRepo) RegisterPushToStartToken(ctx context.Context, tripID, userID, token string, sandbox bool) error {
	if strings.TrimSpace(tripID) == "" {
		return fmt.Errorf("store.RegisterPushToStartToken: empty trip id")
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("store.RegisterPushToStartToken(trip=%s): empty user id", tripID)
	}
	if strings.TrimSpace(token) == "" {
		// P1: report its absence, never its value.
		return fmt.Errorf("store.RegisterPushToStartToken(trip=%s, user=%s): empty token", tripID, userID)
	}
	if _, err := r.pool.Exec(ctx, queryUpsertTripActivityToken, tripID, userID, token, sandbox); err != nil {
		return fmt.Errorf("store.RegisterPushToStartToken(trip=%s, user=%s): %w", tripID, userID, err)
	}
	return nil
}

// DeletePushToStartToken removes one person's registration for one trip,
// reporting whether a row matched. A miss is not an error: the DELETE endpoint
// is idempotent, and a registration somebody else holds must look identical to
// one that never existed so the endpoint cannot be used to probe.
func (r *TripActivityTokenRepo) DeletePushToStartToken(ctx context.Context, tripID, userID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, queryDeleteTripActivityToken, tripID, userID)
	if err != nil {
		return false, fmt.Errorf("store.DeletePushToStartToken(trip=%s, user=%s): %w", tripID, userID, err)
	}
	return tag.RowsAffected() > 0, nil
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
