package trips

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// The deletion settlement (MYR-607, rest-api.md §7.30.10).
//
// NotifyTripDeleted is SettleTrip with two differences, and both of them are
// asserted here: the banner carries `deleted: true`, and the open legs are
// closed even when the end claim was already spent.

func TestNotifyTripDeleted_FansOutWithTheDeletedFlag(t *testing.T) {
	svc, trips, legs, pusher, activities, reval := newTestService(t)
	trips.audience["trip-1"] = TripAudience{
		TripID: "trip-1", VehicleID: "veh-1", OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1", "p2"},
	}
	if _, err := legs.StartLeg(context.Background(), "trip-1", "veh-1", "Grand Canyon", time.Now()); err != nil {
		t.Fatalf("StartLeg: %v", err)
	}

	if err := svc.NotifyTripDeleted(context.Background(), "trip-1"); err != nil {
		t.Fatalf("NotifyTripDeleted: %v", err)
	}

	// THE CARD GOES DOWN FIRST. Same ordering argument SettleTrip makes: a leg
	// card still saying the car is driving to the Grand Canyon is a lie on a
	// lock screen, while a missing banner is only a silence.
	if len(activities.ends) != 1 {
		t.Fatalf("leg Activity ends = %d, want 1 — the card would have been stranded", len(activities.ends))
	}
	open, err := legs.OpenLegsForTrip(context.Background(), "trip-1")
	if err != nil {
		t.Fatalf("OpenLegsForTrip: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("open legs = %d, want 0 — the leg outlived the trip", len(open))
	}

	var ended *push.TripPush
	for i := range pusher.sent {
		if pusher.sent[i].Event == push.TripEventEnded {
			ended = &pusher.sent[i]
		}
	}
	if ended == nil {
		t.Fatalf("no trip_ended was sent: %v", pusher.events())
	}
	if !ended.Deleted {
		t.Error("trip_ended did not carry deleted — the app would leave an ended trip on the list")
	}
	// It is `trip_ended` and NOT a sixth event, deliberately: an unknown
	// `event` routes an installed build to its default arm, which for a
	// lifecycle push is "do nothing".
	if ended.Event != push.TripEventEnded {
		t.Errorf("event = %q, want trip_ended", ended.Event)
	}
	// PARTICIPANTS ONLY. The owner pressed the button.
	if len(ended.UserIDs) != 2 {
		t.Errorf("recipients = %v, want the two participants", ended.UserIDs)
	}
	for _, id := range ended.UserIDs {
		if id == "owner" {
			t.Errorf("the owner was told about their own deletion: %v", ended.UserIDs)
		}
	}

	svc.DrainRevalidation()
	if reval.count() == 0 {
		t.Error("no access revalidation was nudged")
	}
}

// TestNotifyTripDeleted_ClosesLegsEvenWhenTheEndWasAlreadyClaimed is the one
// behaviour that separates this path from SettleTrip.
//
// SettleTrip returns early on a lost claim because a second closing edge has
// nothing left to do. A DELETION is the last moment any of it is possible: the
// rows that address those cards are about to stop existing, and a settlement
// interrupted between its claim and its legs would leave a live Activity on
// every participant's lock screen until ActivityKit's staleness ceiling
// retired it.
func TestNotifyTripDeleted_ClosesLegsEvenWhenTheEndWasAlreadyClaimed(t *testing.T) {
	svc, trips, legs, pusher, activities, _ := newTestService(t)
	trips.audience["trip-1"] = TripAudience{
		TripID: "trip-1", VehicleID: "veh-1", OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1"},
	}
	// Somebody already claimed the end — the sweeper's pass, or an "End trip"
	// tap a second earlier — and was interrupted before it reached the legs.
	claimed, err := trips.ClaimTripEndNow(context.Background(), "trip-1")
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	if _, err := legs.StartLeg(context.Background(), "trip-1", "veh-1", "Grand Canyon", time.Now()); err != nil {
		t.Fatalf("StartLeg: %v", err)
	}

	if err := svc.NotifyTripDeleted(context.Background(), "trip-1"); err != nil {
		t.Fatalf("NotifyTripDeleted: %v", err)
	}

	if len(activities.ends) != 1 {
		t.Errorf("leg Activity ends = %d, want 1 even on a lost claim", len(activities.ends))
	}
	// NO SECOND BANNER. The claim is what makes "at most one trip_ended" hold
	// across the sweeper, the early end and this route at the same instant.
	for _, p := range pusher.sent {
		if p.Event == push.TripEventEnded {
			t.Errorf("a second trip_ended was announced for one trip: %v", pusher.events())
		}
	}
}

// TestSettleTrip_StillSendsNoDeletedFlag pins that the ordinary closing edge is
// unchanged — an ended trip is not a deleted one, and a client that treated the
// two alike would drop a trip the owner can still open.
func TestSettleTrip_StillSendsNoDeletedFlag(t *testing.T) {
	svc, trips, _, pusher, _, _ := newTestService(t)
	trips.audience["trip-1"] = TripAudience{
		TripID: "trip-1", VehicleID: "veh-1", OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1"},
	}

	if err := svc.SettleTrip(context.Background(), "trip-1"); err != nil {
		t.Fatalf("SettleTrip: %v", err)
	}
	if len(pusher.sent) != 1 {
		t.Fatalf("sent %d pushes, want 1: %v", len(pusher.sent), pusher.events())
	}
	if pusher.sent[0].Deleted {
		t.Error("an ordinary end carried deleted:true")
	}
}
