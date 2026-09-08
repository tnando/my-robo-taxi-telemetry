package trips

import (
	"context"
	"errors"
	"testing"
	"time"
)

// MYR-612 — THE CATCH-UP.
//
// A leg's Live Activity is push-to-start, and the fan-out runs ONCE, at the
// instant the leg opens, over whatever tokens are registered then. Registering
// is what a phone does when the `trip_leg_started` push WAKES it — necessarily
// afterwards. Production, 2026-09-08: the only participant's token was written
// at 03:40:27 for a leg that opened at 03:40:24. `go_live_activities` held zero
// leg rows for the rest of the evening: no card for anybody, ever.

// catchUpService wires a service over an already-open leg.
func catchUpService(t *testing.T) (*Service, *fakeLegStore, *fakeActivityPusher) {
	t.Helper()
	trips, legs := newFakeTripStore(), newFakeLegStore()
	trips.audience[testTrip] = TripAudience{
		TripID: testTrip, VehicleID: testVehicle, OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1"},
	}
	trips.names[testTrip] = "DFW to LA"
	activities := &fakeActivityPusher{}
	svc := NewService(trips, legs, Config{Enabled: true}, nil).WithActivities(activities)
	return svc, legs, activities
}

func openTestLeg(t *testing.T, legs *fakeLegStore) Leg {
	t.Helper()
	leg, err := legs.StartLeg(context.Background(), testTrip, testVehicle, sedona, frameBase)
	if err != nil {
		t.Fatal(err)
	}
	return leg
}

// TestRegisteringDuringAnOpenLegRaisesTheCard is the incident, inverted.
func TestRegisteringDuringAnOpenLegRaisesTheCard(t *testing.T) {
	svc, legs, activities := catchUpService(t)
	leg := openTestLeg(t, legs)

	svc.CatchUpLegActivity(context.Background(), testTrip, "p1")

	if len(activities.catchUps) != 1 {
		t.Fatalf("catch-up sends = %v, want one for p1 on %s", activities.catchUps, leg.ID)
	}
	if got, want := activities.catchUps[0], "p1/"+leg.ID; got != want {
		t.Errorf("catch-up = %q, want %q", got, want)
	}
}

// TestRegisteringWithNoOpenLegSendsNothing: the overwhelmingly common case, and
// it must cost one read and no push.
func TestRegisteringWithNoOpenLegSendsNothing(t *testing.T) {
	svc, _, activities := catchUpService(t)

	svc.CatchUpLegActivity(context.Background(), testTrip, "p1")

	if len(activities.catchUps) != 0 {
		t.Errorf("a trip with no open leg raised a card: %v", activities.catchUps)
	}
}

// TestTheCatchUpCarriesNoETA pins MYR-194's rule at this instant. A
// registration is not a frame; the honest answer is that no estimate has been
// computed, and an absent one renders a card with no time rather than a wrong
// one.
func TestTheCatchUpCarriesNoETA(t *testing.T) {
	svc, legs, activities := catchUpService(t)
	openTestLeg(t, legs)

	svc.CatchUpLegActivity(context.Background(), testTrip, "p1")

	if len(activities.catchUpContexts) != 1 {
		t.Fatalf("want one catch-up, got %v", activities.catchUps)
	}
	tc := activities.catchUpContexts[0]
	if tc.ETAMinutes != nil {
		t.Errorf("the catch-up invented an ETA: %d", *tc.ETAMinutes)
	}
	if tc.Status != tripStatusEnroute {
		t.Errorf("status = %q, want %q", tc.Status, tripStatusEnroute)
	}
	if tc.Destination != sedona {
		t.Errorf("destination = %q, want %q — the card must name where the car is going", tc.Destination, sedona)
	}
}

// TestTheCatchUpIsSilentWhenNothingIsWired: a deployment with no APNs key has
// no activity pusher, and this path must not be the one that panics on it.
func TestTheCatchUpIsSilentWhenNothingIsWired(t *testing.T) {
	trips, legs := newFakeTripStore(), newFakeLegStore()
	svc := NewService(trips, legs, Config{Enabled: true}, nil)
	openTestLeg(t, legs)

	svc.CatchUpLegActivity(context.Background(), testTrip, "p1") // must not panic
}

// TestTheCatchUpSurvivesAFailedAudienceRead: the registration has already
// committed, so a failure here is a missing card, never a failed request.
func TestTheCatchUpSurvivesAFailedAudienceRead(t *testing.T) {
	svc, legs, activities := catchUpService(t)
	openTestLeg(t, legs)
	svc.trips.(*fakeTripStore).audErr = errors.New("connection reset")

	svc.CatchUpLegActivity(context.Background(), testTrip, "p1")

	if len(activities.catchUps) != 0 {
		t.Errorf("a card was raised without an audience: %v", activities.catchUps)
	}
}

// TestTheCatchUpIgnoresAClosedLeg: a leg that ended has a card that was ended
// with it, and raising one now would put a stale journey back on a lock screen.
func TestTheCatchUpIgnoresAClosedLeg(t *testing.T) {
	svc, legs, activities := catchUpService(t)
	leg := openTestLeg(t, legs)
	if err := legs.EndLeg(context.Background(), leg.ID, frameBase.Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}

	svc.CatchUpLegActivity(context.Background(), testTrip, "p1")

	if len(activities.catchUps) != 0 {
		t.Errorf("a closed leg raised a card: %v", activities.catchUps)
	}
}
