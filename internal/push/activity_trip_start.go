package push

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RAISING A LEG'S CARD, which is the half of this notifier that has two senders
// and therefore needs a claim.
//
// Split from activity_trip_notifier.go under the 300-line cap, along the seam
// MYR-612 created: an UPDATE and an END address a card that already exists,
// through its own per-Activity token, and cannot double-send by construction. A
// START creates one, from a registry two different callers reach — the leg-open
// fan-out and the registration catch-up, which exists because a phone that
// registers three seconds after a leg opened would otherwise never get a card.
// Everything on this page is about making those two safe together.

// StartLeg push-to-starts one leg's card on every registered phone.
//
// Reports how many cards it asked Apple to create. Zero is an ordinary result —
// a trip whose participants are all on the web, or one nobody has opened on an
// iPhone — and is never an error: the leg's pushes still go out through the
// ordinary notifier, which is what the "a leg that never got a token
// registration still gets its pushes" rule means.
//
// ⚠ THIS FAN-OUT IS NOT THE ONLY SENDER (MYR-612). A phone that registers its
// token DURING an open leg — which is exactly what a phone does when the
// `trip_leg_started` push wakes it, three seconds too late — gets its card from
// the registration catch-up instead. Both claim per (device, leg) before
// sending, so the two cannot raise two cards for one journey.
func (t *TripActivityNotifier) StartLeg(ctx context.Context, tc TripLegContext) int {
	if !t.active() {
		t.logger.Debug("trip activity start skipped",
			slog.String("leg_id", tc.LegID),
			slog.Bool("push_enabled", t.cfg.Enabled),
			slog.Bool("apns_configured", t.sender != nil),
		)
		return 0
	}

	// ONE STATEMENT (MYR-612 review): it claims every registered device of the
	// trip for this leg and hands back exactly what it stamped. The shape it
	// replaces listed the registrations and then claimed once per row, re-running
	// the same membership predicate N further times and discarding the P1 tokens
	// the list had already loaded.
	tokens, err := t.store.ClaimPushToStartForLegAll(ctx, tc.TripID, tc.LegID)
	if err != nil {
		t.logger.Error("trip activity: push-to-start fan-out claim failed",
			slog.String("trip_id", tc.TripID),
			slog.String("error", err.Error()),
		)
		return 0
	}
	if len(tokens) == 0 {
		return 0
	}

	now := t.now()
	var started int
	for _, tok := range tokens {
		started += t.sendClaimed(ctx, tc, tok, now)
	}

	t.logger.Info("trip activity started",
		slog.String("trip_id", tc.TripID),
		slog.String("leg_id", tc.LegID),
		slog.Int("tokens", len(tokens)),
		slog.Int("started", started),
	)
	return started
}

// StartLegForUser raises ONE person's card for a leg that is ALREADY OPEN — the
// MYR-612 catch-up.
//
// WHY IT HAS TO EXIST. The fan-out above runs once, at the instant the leg
// opens, over whatever tokens are registered then. On 2026-09-08 the only
// participant's phone registered its token at 03:40:27, three seconds after the
// leg opened at 03:40:24, because registering is what the phone did on
// RECEIVING the leg-start push. The fan-out had already found an empty registry
// and nothing looked again: no card for anybody, ever, on that leg.
//
// It shares startOne with the fan-out, so it shares the claim, and the two
// cannot double-send however closely they race.
func (t *TripActivityNotifier) StartLegForUser(ctx context.Context, tc TripLegContext, userID string) int {
	if !t.active() || userID == "" {
		return 0
	}
	started := t.startOne(ctx, tc, userID, t.now())
	if started > 0 {
		t.logger.Info("trip activity started late",
			slog.String("trip_id", tc.TripID),
			slog.String("leg_id", tc.LegID),
			slog.String("user_id", userID),
		)
	}
	return started
}

// startOne claims and sends ONE device's push-to-start — the catch-up's path,
// which arrives from an HTTP handler naming one phone and holds no list.
func (t *TripActivityNotifier) startOne(ctx context.Context, tc TripLegContext, userID string, now time.Time) int {
	if !t.allowed(ctx, userID, tc.LegID) {
		return 0
	}
	tok, claimed, err := t.store.ClaimPushToStartForLeg(ctx, tc.TripID, userID, tc.LegID)
	if err != nil {
		t.logger.Error("trip activity: push-to-start claim failed",
			slog.String("leg_id", tc.LegID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return 0
	}
	if !claimed {
		// Already sent for this leg, or the caller is no longer on the trip.
		return 0
	}
	return t.sendClaimed(ctx, tc, tok, now)
}

// sendClaimed delivers ONE already-claimed push-to-start. Returns 1 when a card
// was raised.
//
// ⚠ THE PREFERENCE IS CHECKED AFTER THE CLAIM ON THE FAN-OUT PATH, which is a
// deliberate consequence of claiming the whole trip in one statement: a muted
// recipient's slot is taken and then GIVEN BACK, rather than never taken. The
// net state is identical — no card, no standing claim — and the alternative
// (asking the preference store once per registration before a claim that
// already knows the answer to "who is on this trip") is the N+1 this shape
// exists to remove. The catch-up still asks first, because it names one person
// and has nothing to batch.
func (t *TripActivityNotifier) sendClaimed(
	ctx context.Context, tc TripLegContext, tok ActivityStartToken, now time.Time,
) int {
	if !t.allowed(ctx, tok.UserID, tc.LegID) {
		t.releasePushToStartClaim(ctx, tc, tok.UserID)
		return 0
	}

	err := t.sender.SendActivity(ctx, ActivityNotification{
		ActivityToken: tok.Token,
		Sandbox:       tok.Sandbox,
		Event:         ActivityEventStart,
		ContentState:  tripContentState(tc, now),
		Timestamp:     now,
		// The LEG is REQUIRED in the attributes: without it the created card
		// has no anchor to register its own update token against and can never
		// be updated or ended — and the iOS struct declares it non-optional, so
		// a payload missing it fails the decode and raises no card at all.
		Start: &TripActivityStart{
			TripID:      tc.TripID,
			LegID:       tc.LegID,
			VehicleID:   tc.VehicleID,
			VehicleName: tc.VehicleName,
		},
		// No Alert. The card APPEARING is the announcement, and the
		// `trip_leg_started` banner is already on its way; a third interruption
		// for one fact is what MYR-413 exists to stop.
	})
	switch {
	case err == nil:
		return 1
	case errors.Is(err, ErrUnregistered):
		// THE APP is gone, not a card — this token addresses an installation.
		// The row goes from go_trip_activity_tokens, which is a DIFFERENT table
		// from the one dropActivity touches; see the store file's header for
		// why pointing the ride path at this verdict would delete nothing and
		// retry forever. The claim is NOT released: there is no row left to
		// release it on.
		t.dropPushToStartToken(ctx, tok.Token)
		return 0
	default:
		// EVERY OTHER FAILURE RELEASES THE CLAIM, CANCELLATION INCLUDED
		// (MYR-612 review). This is claim-before-send: the row already says
		// this device has been sent this leg's card, and if that is not true
		// no sender will ever try it again for the rest of the leg. A context
		// deadline or cancellation is not a special case here — it is the most
		// likely transient failure of all, and the least deserving of a
		// permanent consequence.
		t.logger.Warn("trip activity: push-to-start failed",
			slog.String("leg_id", tc.LegID),
			slog.String("user_id", tok.UserID),
			slog.String("push_to_start_token_prefix", tokenPrefix(tok.Token)),
			slog.String("error", err.Error()),
		)
		t.releasePushToStartClaim(ctx, tc, tok.UserID)
		return 0
	}
}

// releasePushToStartClaim hands a claim back after a send that might succeed
// later, on a context detached from the caller's (which the send may have
// consumed).
func (t *TripActivityNotifier) releasePushToStartClaim(ctx context.Context, tc TripLegContext, userID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	if err := t.store.ReleasePushToStartClaim(ctx, tc.TripID, userID, tc.LegID); err != nil {
		t.logger.Error("trip activity: releasing a push-to-start claim failed",
			slog.String("leg_id", tc.LegID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
	}
}
