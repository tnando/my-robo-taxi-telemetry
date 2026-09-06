package push

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The per-card fan-out for trip legs (MYR-602). Split from
// activity_trip_notifier.go so both files stay inside the 300-line cap; that
// file owns the three MOMENTS of a leg's card, this one owns delivering any of
// them to the set of phones holding it.

// legFanOut is one pass over a leg's running Activities: which push shape goes
// out, what instant it claims its state was true at, and whether it opens the
// island.
//
// A struct rather than four positional parameters, for exactly the reason its
// ride twin (activityFanOut) gives: the end PAIR differs from an ordinary pass
// in three of these at once, and a positional bool is how an `end` that alerts
// — or a pair of pushes sharing one `aps.timestamp` — gets shipped by accident.
type legFanOut struct {
	event ActivityEvent
	// at is `aps.timestamp`, PASSED IN rather than read from the clock here.
	// The end pair's ordering depends on the two values differing by a whole
	// second; see TripActivityNotifier.EndLeg.
	at time.Time
	// dismissAt is `aps.dismissal-date`, set only on an `end`.
	dismissAt *time.Time
	// alert opens the Dynamic Island. Set ONLY on the alerting update that
	// precedes an end — buildActivityPayload refuses to write one on an `end`
	// at all (MYR-418), so a caller that put it there would silently get
	// nothing.
	alert *ActivityAlert
}

// fanOutLeg delivers one pass to every Activity running for a leg, and reports
// how many Apple accepted.
func (t *TripActivityNotifier) fanOutLeg(ctx context.Context, tc TripLegContext, spec legFanOut) int {
	if !t.active() {
		return 0
	}

	activities, err := t.store.ActivitiesForLeg(ctx, tc.LegID)
	if err != nil {
		t.logger.Error("trip activity: registry lookup failed",
			slog.String("leg_id", tc.LegID),
			slog.String("error", err.Error()),
		)
		return 0
	}
	if len(activities) == 0 {
		return 0
	}

	state := tripContentState(tc, spec.at)
	var delivered int
	// delivery records WHO Apple accepted, so their rows can be stamped. Only
	// the accepted ones: a card Apple refused is not "recently pushed", and
	// stamping it would hold a permanently failing row past the reaper for
	// nobody — the same rule the ride ticker's markPushed follows.
	delivery := make([]string, 0, len(activities))
	for i := range activities {
		act := activities[i]
		if !t.allowed(ctx, act.UserID, tc.LegID) {
			continue
		}
		err := t.sender.SendActivity(ctx, ActivityNotification{
			ActivityToken: act.Token,
			Sandbox:       act.Sandbox,
			Event:         spec.event,
			ContentState:  state,
			Timestamp:     spec.at,
			DismissalDate: spec.dismissAt,
			Alert:         spec.alert,
		})
		switch {
		case err == nil:
			delivered++
			delivery = append(delivery, act.UserID)
		case errors.Is(err, ErrUnregistered):
			// THE CARD is gone — swiped away, or ended by the app — not the
			// phone and not the app. This is the UPDATE token, so the row goes
			// from go_live_activities, which is the opposite table from the one
			// a push-to-start rejection touches.
			t.dropLegActivity(ctx, act.Token)
		default:
			t.logger.Warn("trip activity: send failed",
				slog.String("leg_id", tc.LegID),
				slog.String("activity_token_prefix", tokenPrefix(act.Token)),
				slog.String("error", err.Error()),
			)
		}
	}

	t.markLegPushed(ctx, tc.LegID, delivery)

	t.logger.Info("trip activity pushed",
		slog.String("trip_id", tc.TripID),
		slog.String("leg_id", tc.LegID),
		slog.String("event", string(spec.event)),
		slog.String("status", tc.Status),
		slog.Int("activities", len(activities)),
		slog.Int("delivered", delivered),
		slog.Bool("has_eta", state.ETA != nil),
		slog.Bool("alerting", spec.alert != nil),
	)
	return delivered
}

// markLegPushed stamps the rows this pass delivered to.
//
// WITHOUT IT A LIVE CARD IS REAPED OUT FROM UNDER ITSELF. go_live_activities is
// swept 24 hours after each row's last WRITE, and `updated_at` is what makes
// that horizon mean "last touched" rather than "registered": the ride path
// keeps it true by stamping every delivered pass, and the leg path did not
// stamp at all. A card on a long drive — a road trip's whole first day — had
// its registration hard-deleted while it was still being pushed to, and the end
// push then had no address at all: the card ran on to ActivityKit's own ceiling
// still saying the car was driving somewhere it reached hours before.
//
// Non-fatal and detached from the send's context, exactly as the ride ticker's
// markPushed is: a failed stamp costs a reaping horizon, never a pass of
// updates.
func (t *TripActivityNotifier) markLegPushed(ctx context.Context, legID string, userIDs []string) {
	if len(userIDs) == 0 {
		return
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	if _, err := t.store.MarkLegActivitiesPushed(markCtx, legID, userIDs); err != nil {
		t.logger.Error("trip activity: mark pushed failed; the reaper may remove a live card",
			slog.String("leg_id", legID),
			slog.Int("activities", len(userIDs)),
			slog.String("error", err.Error()))
	}
}

// dropLegActivity removes an UPDATE token APNs permanently rejected.
func (t *TripActivityNotifier) dropLegActivity(ctx context.Context, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	if err := t.store.DeleteActivityToken(ctx, token); err != nil {
		t.logger.Error("trip activity: delete rejected activity token failed",
			slog.String("activity_token_prefix", tokenPrefix(token)),
			slog.String("error", err.Error()),
		)
		return
	}
	t.logger.Info("trip activity: deleted unregistered activity",
		slog.String("activity_token_prefix", tokenPrefix(token)),
	)
}

// legEndAlert is the copy on the alerting update that precedes a leg's `end`.
//
// It exists because that update is the SOLE announcement of the leg ending on
// the card surface — the `end` itself expands nothing (MYR-418) — and because
// the island's expansion is the whole point: a participant watching a card
// should see it change, not merely find it changed.
//
// SAME PAYLOAD POLICY AS THE BANNERS, and the same argued carve-out: the place
// name is already on this very card in `destination`, so withholding it from
// the two lines beside it would be theatre. The TRIP NAME is not interpolated,
// here as everywhere outside the content-state.
//
// It forks on the two endings because they say different things to a person
// watching: `arrived` is the news the card existed to deliver, while
// `completed` is a leg that stopped short — the car parked elsewhere or its
// route was cleared — and announcing that as an arrival would be a small lie on
// the one surface that cannot be corrected afterwards.
func legEndAlert(tc TripLegContext) *ActivityAlert {
	car := tripVehicleLabel(tc.VehicleName)
	if tc.Status == tripStatusArrived {
		return &ActivityAlert{
			Title: car + " has arrived",
			Body:  arrivedAt(tc.Destination),
		}
	}
	return &ActivityAlert{
		Title: car + " has parked",
		Body:  "The drive has ended.",
	}
}
