package main

import (
	"context"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/push"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// THE LEG CARD'S TWO REGISTRIES, joined onto one interface.
//
// Split from wiring_trips_live.go so both stay inside the 300-line cap, and
// kept whole as a unit because the two tables it spans are the one pair this
// codebase most needs read together: `go_trip_activity_tokens` holds a
// PUSH-TO-START token addressing the APP, `go_live_activities` holds a
// per-Activity UPDATE token addressing ONE RUNNING CARD, and a 410 means
// something different about each. This adapter is the only place that knows
// both, which is exactly where the routing of each rejection belongs.

// tripActivityStoreAdapter joins the TWO registries a leg's card needs onto one
// interface: the push-to-start tokens and the per-Activity update tokens.
//
// They are two tables and two meanings of a 410 — see
// internal/store/trip_activity_token_repo.go — which is exactly why they are
// adapted together HERE rather than passed as one repo: this is the only place
// that knows both, and it is where the routing of each rejection is decided.
type tripActivityStoreAdapter struct {
	tokens     *store.TripActivityTokenRepo
	activities *store.LiveActivityRepo
}

func (a *tripActivityStoreAdapter) PushToStartTokensForTrip(
	ctx context.Context, tripID string,
) ([]push.ActivityStartToken, error) {
	rows, err := a.tokens.PushToStartTokensForTrip(ctx, tripID)
	if err != nil {
		return nil, fmt.Errorf("trips: list push-to-start tokens: %w", err)
	}
	out := make([]push.ActivityStartToken, 0, len(rows))
	for _, row := range rows {
		out = append(out, push.ActivityStartToken{
			UserID: row.UserID, Token: row.PushToStartToken, Sandbox: row.Sandbox,
		})
	}
	return out, nil
}

// ClaimPushToStartForLeg is the per-(device, leg) claim both push-to-start
// senders take before sending (MYR-612).
func (a *tripActivityStoreAdapter) ClaimPushToStartForLeg(
	ctx context.Context, tripID, userID, legID string,
) (push.ActivityStartToken, bool, error) {
	row, claimed, err := a.tokens.ClaimPushToStartForLeg(ctx, tripID, userID, legID)
	if err != nil {
		return push.ActivityStartToken{}, false, fmt.Errorf("trips: claim push-to-start: %w", err)
	}
	if !claimed {
		return push.ActivityStartToken{}, false, nil
	}
	return push.ActivityStartToken{
		UserID: row.UserID, Token: row.PushToStartToken, Sandbox: row.Sandbox,
	}, true, nil
}

func (a *tripActivityStoreAdapter) ReleasePushToStartClaim(ctx context.Context, tripID, userID, legID string) error {
	if err := a.tokens.ReleasePushToStartClaim(ctx, tripID, userID, legID); err != nil {
		return fmt.Errorf("trips: release push-to-start claim: %w", err)
	}
	return nil
}

func (a *tripActivityStoreAdapter) DeleteRejectedPushToStartToken(ctx context.Context, token string) error {
	if err := a.tokens.DeleteRejectedPushToStartToken(ctx, token); err != nil {
		return fmt.Errorf("trips: delete rejected push-to-start token: %w", err)
	}
	return nil
}

func (a *tripActivityStoreAdapter) ActivitiesForLeg(ctx context.Context, legID string) ([]push.Activity, error) {
	rows, err := a.activities.ActivitiesForLeg(ctx, legID)
	if err != nil {
		return nil, fmt.Errorf("trips: list leg activities: %w", err)
	}
	out := make([]push.Activity, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, push.Activity{
			// THE LEG ANCHOR, in its own field. It used to ride the
			// RideRequestID slot on the argument that nothing read it — see
			// push.Activity.TripLegID for why that stopped being good enough.
			TripLegID: row.TripLegID,
			UserID:    row.UserID,
			Token:     row.ActivityPushToken,
			Sandbox:   row.Sandbox,
		})
	}
	return out, nil
}

func (a *tripActivityStoreAdapter) EndActivitiesForLeg(ctx context.Context, legID string) (int64, error) {
	n, err := a.activities.EndActivitiesForLeg(ctx, legID)
	if err != nil {
		return 0, fmt.Errorf("trips: end leg activities: %w", err)
	}
	return n, nil
}

func (a *tripActivityStoreAdapter) MarkLegActivitiesPushed(
	ctx context.Context, legID string, userIDs []string,
) (int64, error) {
	n, err := a.activities.MarkLegActivitiesPushed(ctx, legID, userIDs)
	if err != nil {
		return 0, fmt.Errorf("trips: mark leg activities pushed: %w", err)
	}
	return n, nil
}

func (a *tripActivityStoreAdapter) DeleteActivityToken(ctx context.Context, token string) error {
	if err := a.activities.DeleteActivityToken(ctx, token); err != nil {
		return fmt.Errorf("trips: delete rejected activity token: %w", err)
	}
	return nil
}
