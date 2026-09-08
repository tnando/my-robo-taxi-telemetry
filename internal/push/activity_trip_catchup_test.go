package push

import (
	"context"
	"errors"
	"testing"
)

// MYR-612 — TWO SENDERS, ONE CARD.
//
// The leg-open fan-out and the registration catch-up both raise push-to-start
// Live Activities, because a phone that registers its token three seconds after
// a leg opened would otherwise never get one — which on 2026-09-08 was every
// phone on the trip. Two senders on one lock screen is a duplicate-card risk,
// so both claim per (device, leg) before sending and the claim is what makes
// them safe.

// TestTheCatchUpRaisesACardForOneDevice.
func TestTheCatchUpRaisesACardForOneDevice(t *testing.T) {
	store := &fakeTripActivityStore{tokens: []ActivityStartToken{
		{UserID: "user-a", Token: "pts-a"},
		{UserID: "user-b", Token: "pts-b"},
	}}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())

	if got := n.StartLegForUser(context.Background(), tripLegFixture(), "user-b"); got != 1 {
		t.Fatalf("started %d cards, want 1", got)
	}
	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d pushes, want 1 — the catch-up addresses ONE phone", len(sent))
	}
	if sent[0].ActivityToken != "pts-b" {
		t.Errorf("sent to %q, want the registering device's token", sent[0].ActivityToken)
	}
	if sent[0].Event != ActivityEventStart {
		t.Errorf("event = %q, want a push-to-start", sent[0].Event)
	}
	if sent[0].Start == nil || sent[0].Start.LegID != "leg-1" {
		t.Error("the LEG anchor is required in the attributes; without it the card can never be updated or ended")
	}
	if sent[0].Alert != nil {
		t.Error("the catch-up must not alert: the trip_leg_started banner already went out")
	}
}

// TestTheFanOutAndTheCatchUpCannotBothSend is the property the stamp exists for.
func TestTheFanOutAndTheCatchUpCannotBothSend(t *testing.T) {
	store := &fakeTripActivityStore{tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-a"}}}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())
	tc := tripLegFixture()

	if got := n.StartLeg(context.Background(), tc); got != 1 {
		t.Fatalf("fan-out started %d, want 1", got)
	}
	if got := n.StartLegForUser(context.Background(), tc, "user-a"); got != 0 {
		t.Fatalf("the catch-up sent a SECOND card for the same leg to the same phone (%d)", got)
	}
	if len(sender.Sent()) != 1 {
		t.Fatalf("pushes = %d, want 1 — two Live Activities for one journey", len(sender.Sent()))
	}
}

// TestTheCatchUpIsIdempotentOnRepeatedRegistration. An app re-POSTs its
// unchanged token on every foreground; the prod row for the incident was
// updated five minutes after it was created. Each of those must not raise a
// card.
func TestTheCatchUpIsIdempotentOnRepeatedRegistration(t *testing.T) {
	store := &fakeTripActivityStore{tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-a"}}}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())
	tc := tripLegFixture()

	for i := 0; i < 5; i++ {
		n.StartLegForUser(context.Background(), tc, "user-a")
	}
	if len(sender.Sent()) != 1 {
		t.Fatalf("pushes = %d over five registrations, want 1", len(sender.Sent()))
	}
}

// TestANewLegClaimsAgain: the stamp is per LEG, not per trip. A trip runs for
// days and contains a dozen legs, each with its own card.
func TestANewLegClaimsAgain(t *testing.T) {
	store := &fakeTripActivityStore{tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-a"}}}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())

	first := tripLegFixture()
	n.StartLeg(context.Background(), first)
	second := tripLegFixture()
	second.LegID = "leg-2"
	n.StartLeg(context.Background(), second)

	if len(sender.Sent()) != 2 {
		t.Fatalf("pushes = %d, want one per leg", len(sender.Sent()))
	}
}

// TestATransientSendFailureReleasesTheClaim. Claim-before-send would otherwise
// turn one APNs hiccup into a permanently card-less leg for that device: the
// row would read "already started" while no card exists.
func TestATransientSendFailureReleasesTheClaim(t *testing.T) {
	store := &fakeTripActivityStore{tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-a"}}}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())
	sender.Err = errors.New("503 from APNs")

	tc := tripLegFixture()
	if got := n.StartLeg(context.Background(), tc); got != 0 {
		t.Fatalf("started %d despite a failing sender", got)
	}
	store.mu.Lock()
	released := append([]string(nil), store.released...)
	store.mu.Unlock()
	if len(released) != 1 {
		t.Fatalf("claims released = %v, want the failed one back", released)
	}

	// And the retry now succeeds.
	sender.Err = nil
	if got := n.StartLegForUser(context.Background(), tc, "user-a"); got != 1 {
		t.Fatalf("the retry started %d cards, want 1", got)
	}
}

// TestA410DeletesTheRowAndReleasesNothing: that verdict is about the APP, not a
// card, and the row it addresses is gone — there is nothing left to release.
func TestA410DeletesTheRowAndReleasesNothing(t *testing.T) {
	store := &fakeTripActivityStore{tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-a"}}}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())
	sender.Err = ErrUnregistered

	n.StartLeg(context.Background(), tripLegFixture())

	store.mu.Lock()
	dropped, released := append([]string(nil), store.droppedPTS...), append([]string(nil), store.released...)
	store.mu.Unlock()
	if len(dropped) != 1 || dropped[0] != "pts-a" {
		t.Errorf("dropped = %v, want the unregistered push-to-start token", dropped)
	}
	if len(released) != 0 {
		t.Errorf("released = %v, want none — the row is deleted", released)
	}
}

// TestACancelledSendReleasesThePerDeviceClaim — MYR-612 review.
//
// CLAIM BEFORE SEND is what keeps the two senders from raising two cards, and
// its whole cost is that a claim which is not followed by a card is permanent:
// no sender tries that device again for the rest of the leg. A context
// cancellation is the most likely transient failure of all — the catch-up runs
// off a request a suspending phone abandons — and the least deserving of that
// consequence, so it releases like any other failure that might succeed later.
func TestACancelledSendReleasesThePerDeviceClaim(t *testing.T) {
	store := &fakeTripActivityStore{tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-a"}}}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())
	sender.Err = context.Canceled
	tc := tripLegFixture()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := n.StartLegForUser(ctx, tc, "user-a"); got != 0 {
		t.Fatalf("started %d cards on a cancelled send, want 0", got)
	}

	want := tc.TripID + "/user-a/" + tc.LegID
	if len(store.released) != 1 || store.released[0] != want {
		t.Fatalf("released = %v, want [%s] — the claim outlived a card that was "+
			"never raised, and nothing will send to that phone again this leg",
			store.released, want)
	}

	// And the release is real: a later attempt can claim and send.
	sender.Err = nil
	if got := n.StartLegForUser(context.Background(), tc, "user-a"); got != 1 {
		t.Fatalf("the retry started %d cards, want 1", got)
	}
	if len(sender.Sent()) != 2 {
		t.Errorf("pushes = %d, want 2 (the cancelled attempt and the retry)", len(sender.Sent()))
	}
}
