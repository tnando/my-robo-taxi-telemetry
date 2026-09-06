package push

import "context"

// THE SEAM the leg-card sender persists through: two registries, adapted onto
// one interface.
//
// Split from activity_trip_notifier.go so both stay inside the 300-line cap,
// and it is worth its own page for the reason the interface itself is worth
// having: the two registries hold DIFFERENT KINDS OF TOKEN and a `410` means a
// different thing about each, so the six methods below are the one place where
// the distinction is stated in a form a compiler checks.

// TripActivityStore is the send path's view of the two registries a leg's card
// needs. Declared here at the consumer site, like ActivityStore beside it, so
// internal/push keeps its independence from internal/store.
type TripActivityStore interface {
	// PushToStartTokensForTrip lists the trip's push-to-start registrations —
	// participants and the owner. An empty result is ordinary.
	PushToStartTokensForTrip(ctx context.Context, tripID string) ([]ActivityStartToken, error)
	// DeleteRejectedPushToStartToken drops a push-to-start token APNs
	// permanently rejected. NOT DeleteActivityToken: see the two tables'
	// difference in internal/store/trip_activity_token_repo.go.
	DeleteRejectedPushToStartToken(ctx context.Context, token string) error
	// ActivitiesForLeg lists the still-live Activities anchored to one leg.
	ActivitiesForLeg(ctx context.Context, legID string) ([]Activity, error)
	// EndActivitiesForLeg tombstones them, after the final `end` push.
	EndActivitiesForLeg(ctx context.Context, legID string) (int64, error)
	// MarkLegActivitiesPushed stamps updated_at on the rows a fan-out just
	// delivered to, which is what keeps the 24-hour reaper away from a card
	// that is still on somebody's lock screen. See fanOutLeg.
	MarkLegActivitiesPushed(ctx context.Context, legID string, userIDs []string) (int64, error)
	// DeleteActivityToken drops an UPDATE token APNs permanently rejected.
	DeleteActivityToken(ctx context.Context, token string) error
}

// ActivityStartToken is one registered push-to-start token, the consumer-site
// view of a go_trip_activity_tokens row.
type ActivityStartToken struct {
	UserID string
	// Token is the raw ActivityKit push-to-start token. P1 CAPABILITY — never
	// log in full, never echo.
	Token   string
	Sandbox bool
}
