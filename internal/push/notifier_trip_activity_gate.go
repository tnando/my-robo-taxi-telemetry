package push

import (
	"context"
	"log/slog"
)

// THE LEG BANNER IS FOR PHONES THAT HAVE NO CARD (MYR-620).
//
// THE FIELD REPORT, 2026-09-08. Ten "Tesla is on the move — Heading to Element
// by Marriott Sedona." banners on one lock screen in 59 minutes, and the
// client's reading of the screenshot was *"this should be moving into dynamic
// island"*. MYR-612's debounce stops most of the repeats; this stops the
// duplication that was there from the first one.
//
// A LEG ALREADY ANNOUNCES ITSELF TWICE. The leg-open fan-out push-to-starts a
// Live Activity on every phone registered for the trip — the card appearing IS
// the announcement, which is why startOne deliberately attaches no alert — and
// the lifecycle notifier sends an ordinary banner carrying the same sentence in
// prose. On a phone holding both, the banner is not merely redundant: it takes
// the strip of screen the card wants, so the very push that duplicates the card
// is also the thing standing in front of it. That is the MYR-413 argument
// exactly, made about a leg instead of a ride.
//
// THE RULE. A leg banner is skipped when the RECIPIENT holds a push-to-start
// registration for that TRIP. Everything else keeps its banner:
//
//   - A phone with notifications allowed and Live Activities OFF never
//     registers a token, so it is not in the registry and it is told in prose.
//     That is the whole reason the banner still exists.
//   - The THREE LIFECYCLE pushes (`trip_added`, `trip_started`, `trip_ended`)
//     are untouched. They are not about a leg, no card announces them, and a
//     trip that ended is precisely when a card is going away.
//   - A registration that APNs has since rejected with a 410 is DELETED from
//     the registry, so its owner reads as token-less and keeps every banner. A
//     phone whose app is gone must never be left dark.
//
// THE GRAIN IS (TRIP, USER), because that is the registry's own primary key: a
// push-to-start token is stored once per (trip, user) and rotated in place. A
// person signed in on two phones, one of which has Live Activities disabled,
// therefore loses the banner on the second — the same trade `go_live_activities`
// makes on the ride surface, and the reason the registry is keyed that way is
// that ActivityKit rotates the token, so accumulating a row per device would
// mean two cards for one journey on one lock screen.

// TripActivityPresenceStore answers whether one party's phone is registered to
// receive this trip's leg Live Activities.
//
// Declared at the consumer site, like ActivityPresenceStore beside it, so
// internal/push keeps its independence from internal/store. Satisfied by
// *store.TripActivityTokenRepo directly — the signature is the repository
// method's, so the cmd wiring needs no adapter.
type TripActivityPresenceStore interface {
	HasPushToStartToken(ctx context.Context, tripID, userID string) (bool, error)
}

// holdsLegActivity reports whether this recipient's Live Activity is already
// delivering the news this banner carries.
//
// IT FAILS OPEN in all of its failure modes — no store wired, no trip, a
// delivery that is not a leg event, a lookup error — for the same reason the
// ride gate next door does, and this package's standing rule: a duplicate
// notification is a minor annoyance to a human, a missing one is somebody who
// was never told their car set off.
func (n *Notifier) holdsLegActivity(ctx context.Context, d delivery) bool {
	if !d.legActivity || n.stores.tripActivities == nil || d.tripPush == nil {
		return false
	}
	tripID := d.tripPush.TripID
	if tripID == "" {
		return false
	}

	registered, err := n.stores.tripActivities.HasPushToStartToken(ctx, tripID, d.userID)
	if err != nil {
		n.logger.Error("push: push-to-start registry lookup failed; sending the banner anyway",
			slog.String("topic", d.topic),
			slog.String("trip_id", tripID),
			slog.String("user_id", d.userID),
			slog.String("error", err.Error()),
		)
		return false
	}
	if !registered {
		return false
	}

	// P0 throughout — two opaque cuids and a topic. Info rather than Debug for
	// the same reason the other two gates are: "the notification I expected
	// never arrived" is a support question and this line answers it.
	n.logger.Info("push suppressed by leg live activity",
		slog.String("topic", d.topic),
		slog.String("trip_id", tripID),
		slog.String("user_id", d.userID),
	)
	return true
}
