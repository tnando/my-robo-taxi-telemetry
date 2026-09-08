package push

import (
	"context"
	"errors"
	"testing"
)

// fakeTripPresence is the push-to-start registry, as the leg-banner gate sees
// it: a set of (trip, user) pairs, and an error to fail it with.
type fakeTripPresence struct {
	registered map[string]bool
	err        error
	calls      int
}

func (f *fakeTripPresence) HasPushToStartToken(_ context.Context, tripID, userID string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.registered[tripID+"/"+userID], nil
}

// legBannerFixture wires a notifier over two recipients — one holding a
// push-to-start token for the trip, one not.
func legBannerFixture(t *testing.T) (*Notifier, *FakeSender, *fakeTripPresence) {
	t.Helper()
	n, sender := newTripNotifier(t, DefaultPrefs())
	presence := &fakeTripPresence{registered: map[string]bool{"trip-1/user-a": true}}
	return n.WithTripActivityPresence(presence), sender, presence
}

func legStartedPush() TripPush {
	return TripPush{
		TripID: "trip-1", VehicleID: "veh-1", LegID: "leg-1",
		Event: TripEventLegStarted, DestinationName: "Element by Marriott Sedona",
		UserIDs: []string{"user-a", "user-b"},
	}
}

// TestALegBannerSkipsAPhoneThatGetsTheCard — MYR-620.
//
// The client's screenshot showed ten "Tesla is on the move — Heading to Element
// by Marriott Sedona." banners in an hour, and his reading was "this should be
// moving into dynamic island". A phone registered for the trip's push-to-start
// IS getting the island: the card appearing is the announcement, and the banner
// on top of it is the thing standing in front of the card.
func TestALegBannerSkipsAPhoneThatGetsTheCard(t *testing.T) {
	n, sender, presence := legBannerFixture(t)

	n.NotifyTrip(context.Background(), legStartedPush())

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d banners, want 1 — only the token-less phone is told in prose", len(sent))
	}
	if sent[0].DeviceToken != "token-b" {
		t.Errorf("banner went to %q, want the token-less recipient's device", sent[0].DeviceToken)
	}
	if presence.calls != 2 {
		t.Errorf("registry consulted %d times, want once per recipient", presence.calls)
	}

	// AND THE ARRIVAL FOLLOWS THE SAME RULE: the card's alerting end is the
	// announcement for a phone that has one.
	arrived := legStartedPush()
	arrived.Event = TripEventLegArrived
	n.NotifyTrip(context.Background(), arrived)
	got := sender.Sent()
	if len(got) != 2 {
		t.Fatalf("sent %d banners in total, want 2 (one departure, one arrival)", len(got))
	}
	if got[1].DeviceToken != "token-b" {
		t.Errorf("the arrival banner went to %q, want the token-less phone", got[1].DeviceToken)
	}
}

// TestTheLifecyclePushesAreNotGated: `trip_added` / `trip_started` /
// `trip_ended` are not about a leg, no card announces them, and `trip_ended` is
// precisely when a card is going away.
func TestTheLifecyclePushesAreNotGated(t *testing.T) {
	n, sender, _ := legBannerFixture(t)

	events := []TripEvent{TripEventAdded, TripEventStarted, TripEventEnded}
	for _, event := range events {
		n.NotifyTrip(context.Background(), TripPush{
			TripID: "trip-1", VehicleID: "veh-1", Event: event,
			UserIDs: []string{"user-a", "user-b"},
		})
	}
	if got, want := len(sender.Sent()), 2*len(events); got != want {
		t.Errorf("lifecycle banners = %d, want %d (both phones, every event) — "+
			"no card carries this news", got, want)
	}
}

// TestTheLegBannerGateFailsOpen: no store, and a store that errors. A duplicate
// banner is an annoyance; a missing one is somebody never told their car set
// off.
func TestTheLegBannerGateFailsOpen(t *testing.T) {
	t.Run("no registry wired", func(t *testing.T) {
		n, sender := newTripNotifier(t, DefaultPrefs())
		n.NotifyTrip(context.Background(), legStartedPush())
		if got := len(sender.Sent()); got != 2 {
			t.Errorf("banners = %d, want 2 — an unwired gate suppresses nothing", got)
		}
	})

	t.Run("the lookup fails", func(t *testing.T) {
		n, sender, presence := legBannerFixture(t)
		presence.err = errors.New("pool timeout")
		n.NotifyTrip(context.Background(), legStartedPush())
		if got := len(sender.Sent()); got != 2 {
			t.Errorf("banners = %d, want 2 — a database hiccup must not silence a departure", got)
		}
	})
}
