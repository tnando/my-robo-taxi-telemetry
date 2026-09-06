package trips

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/push"
)

func newTestService(t *testing.T) (*Service, *fakeTripStore, *fakeLegStore, *fakePusher, *fakeActivityPusher, *fakeRevalidator) {
	t.Helper()
	trips, legs := newFakeTripStore(), newFakeLegStore()
	pusher, activities, reval := &fakePusher{}, &fakeActivityPusher{}, &fakeRevalidator{}
	svc := NewService(trips, legs, Config{Enabled: true, Dwell: 0}, nil).
		WithPushes(pusher).
		WithActivities(activities).
		WithRevalidator(reval)
	svc.now = func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
	return svc, trips, legs, pusher, activities, reval
}

func TestSweeper_AnnouncesEachEdgeOnceToParticipants(t *testing.T) {
	svc, trips, _, pusher, _, reval := newTestService(t)
	trips.audience["trip-1"] = TripAudience{
		TripID: "trip-1", VehicleID: "veh-1", OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1", "p2"},
	}
	trips.toStart = []string{"trip-1"}

	sweeper := NewSweeper(svc, nil)
	started, ended := sweeper.SweepOnce(context.Background())
	if started != 1 || ended != 0 {
		t.Fatalf("first pass started=%d ended=%d, want 1/0", started, ended)
	}

	// A SECOND PASS OVER THE SAME TRIP MUST ANNOUNCE NOTHING. The stamp is the
	// only thing standing between one banner and one banner per minute forever.
	started, _ = sweeper.SweepOnce(context.Background())
	if started != 0 {
		t.Errorf("second pass started %d trips, want 0 — the claim is not idempotent", started)
	}

	sent := pusher.sent
	if len(sent) != 1 {
		t.Fatalf("sent %d pushes, want 1: %v", len(sent), pusher.events())
	}
	if sent[0].Event != push.TripEventStarted {
		t.Errorf("event = %q, want trip_started", sent[0].Event)
	}
	// PARTICIPANTS ONLY. The owner set the window; telling them their own trip
	// started is the noise the self-ride suppression exists to remove.
	if len(sent[0].UserIDs) != 2 {
		t.Errorf("recipients = %v, want the two participants and not the owner", sent[0].UserIDs)
	}
	for _, id := range sent[0].UserIDs {
		if id == "owner" {
			t.Errorf("the owner was told their own trip started: %v", sent[0].UserIDs)
		}
	}
	// The nudge is asynchronous and coalesced (see revalidate.go), so the
	// assertion waits for it rather than sleeping.
	svc.DrainRevalidation()
	if reval.count() == 0 {
		t.Error("no access revalidation was nudged; participants' sockets would stay " +
			"masked as plain viewers for up to a minute after their phones buzzed")
	}
}

// TestSweeper_ClaimsEndingsBeforeStarts covers the trip whose whole window
// elapsed while the process was down: it is BOTH unstarted and ended, and
// announcing the start first would tell everybody the trip had begun and then,
// milliseconds later, that it had finished.
func TestSweeper_ClaimsEndingsBeforeStarts(t *testing.T) {
	svc, trips, _, pusher, _, _ := newTestService(t)
	trips.audience["trip-elapsed"] = TripAudience{
		TripID: "trip-elapsed", VehicleID: "veh-1", OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1"},
	}
	// The same trip is queued on BOTH edges, which is what the two claim
	// statements would independently match for an elapsed window.
	trips.toStart = []string{"trip-elapsed"}
	trips.toEnd = []string{"trip-elapsed"}

	NewSweeper(svc, nil).SweepOnce(context.Background())

	got := pusher.events()
	if len(got) != 2 || got[0] != push.TripEventEnded {
		t.Fatalf("pushes = %v, want the ENDED one first", got)
	}
}

// TestSweeper_EndingClosesOpenLegsFirst pins the ordering inside a closing edge.
// A card saying "heading to the Grand Canyon" that outlives its trip is a lie on
// a lock screen, and it must be taken down before the trip announcement rather
// than after it.
func TestSweeper_EndingClosesOpenLegsFirst(t *testing.T) {
	svc, trips, legs, pusher, activities, _ := newTestService(t)
	trips.audience["trip-1"] = TripAudience{
		TripID: "trip-1", VehicleID: "veh-1", OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1"},
	}
	trips.toEnd = []string{"trip-1"}
	if _, err := legs.StartLeg(context.Background(), "trip-1", "veh-1", "Grand Canyon", svc.now()); err != nil {
		t.Fatalf("seed leg: %v", err)
	}

	NewSweeper(svc, nil).SweepOnce(context.Background())

	if len(activities.ends) != 1 {
		t.Fatalf("ended %d leg cards, want 1", len(activities.ends))
	}
	// A leg still open when its window closes ended WITHOUT arrival evidence by
	// definition — the car was still driving somewhere when the trip ran out of
	// time — so its final card says the drive ended, not that it arrived.
	if got := activities.ends[0].Status; got != tripStatusCompleted {
		t.Errorf("final card status = %q, want %q", got, tripStatusCompleted)
	}
	for _, p := range pusher.sent {
		if p.Event == push.TripEventLegArrived {
			t.Error("a trip_leg_arrived fired for a leg that was cut short by its window")
		}
	}
	open, _ := legs.OpenLegsForTrip(context.Background(), "trip-1")
	if len(open) != 0 {
		t.Errorf("%d legs left open after the window closed", len(open))
	}
}

// TestSettleTrip_IsIdempotent covers the double tap on End trip, and the early
// end racing the sweeper's own pass.
func TestSettleTrip_IsIdempotent(t *testing.T) {
	svc, trips, _, pusher, _, _ := newTestService(t)
	trips.audience["trip-1"] = TripAudience{
		TripID: "trip-1", VehicleID: "veh-1", ParticipantUserIDs: []string{"p1"},
	}

	for range 3 {
		if err := svc.SettleTrip(context.Background(), "trip-1"); err != nil {
			t.Fatalf("SettleTrip: %v", err)
		}
	}
	if got := len(pusher.sent); got != 1 {
		t.Errorf("sent %d pushes for three settles, want 1: %v", got, pusher.events())
	}
}

// TestSweeper_ClaimFailureIsQuietAndRetries. A database blip must cost one
// interval of staleness and nothing else — never a wedged ticker, and never a
// stamp burned on a pass that announced nothing.
func TestSweeper_ClaimFailureIsQuietAndRetries(t *testing.T) {
	svc, trips, _, pusher, _, _ := newTestService(t)
	trips.audience["trip-1"] = TripAudience{TripID: "trip-1", VehicleID: "veh-1",
		ParticipantUserIDs: []string{"p1"}}
	trips.toStart = []string{"trip-1"}
	trips.claimErr = errors.New("connection reset")

	sweeper := NewSweeper(svc, nil)
	if started, ended := sweeper.SweepOnce(context.Background()); started != 0 || ended != 0 {
		t.Fatalf("a failed pass reported started=%d ended=%d, want 0/0", started, ended)
	}
	if len(pusher.sent) != 0 {
		t.Fatalf("a failed pass sent %d pushes", len(pusher.sent))
	}

	trips.claimErr = nil
	if started, _ := sweeper.SweepOnce(context.Background()); started != 1 {
		t.Errorf("the next pass started %d trips, want 1 — the blip must not have "+
			"consumed the claim", started)
	}
}

// TestNotifyTripAdded_NudgesTheRemask: somebody added to an ALREADY OPEN window
// was a plain viewer a moment ago, and the app they are about to open expects a
// live map.
func TestNotifyTripAdded_NudgesTheRemask(t *testing.T) {
	svc, trips, _, pusher, _, reval := newTestService(t)
	trips.audience["trip-1"] = TripAudience{TripID: "trip-1", VehicleID: "veh-1"}

	if err := svc.NotifyTripAdded(context.Background(), "trip-1", []string{"p9"}); err != nil {
		t.Fatalf("NotifyTripAdded: %v", err)
	}
	if len(pusher.sent) != 1 || pusher.sent[0].Event != push.TripEventAdded {
		t.Fatalf("pushes = %v, want one trip_added", pusher.events())
	}
	if pusher.sent[0].VehicleID != "veh-1" {
		t.Errorf("vehicleId = %q; the deep link must be able to name the car",
			pusher.sent[0].VehicleID)
	}
	svc.DrainRevalidation()
	if reval.count() == 0 {
		t.Error("no revalidation nudge")
	}
}

// TestNotifyTripAdded_EmptyAudienceSendsNothing. A PATCH that removed people
// rather than adding any must not produce a banner addressed to nobody.
func TestNotifyTripAdded_EmptyAudienceSendsNothing(t *testing.T) {
	svc, _, _, pusher, _, _ := newTestService(t)
	if err := svc.NotifyTripAdded(context.Background(), "trip-1", nil); err != nil {
		t.Fatalf("NotifyTripAdded: %v", err)
	}
	if len(pusher.sent) != 0 {
		t.Errorf("sent %d pushes to nobody", len(pusher.sent))
	}
}

// TestSweepOnce_CoalescesTheRemaskNudge is the bound on the re-mask sweeps a
// pass can cost.
//
// Revalidator.SweepOnce is GLOBAL — it walks every connected session on the
// hub — so calling it once per claimed trip means a pass that claims a backlog
// of elapsed windows (the shape an outage produces, up to SweepLimit per
// direction) runs hundreds of identical fleet-wide sweeps inside one tick. The
// single-flight collapses them: at most one running and one trailing, and the
// trailing one is guaranteed to start after the last edge was written.
func TestSweepOnce_CoalescesTheRemaskNudge(t *testing.T) {
	svc, trips, _, _, _, reval := newTestService(t)

	const n = 25
	for i := 0; i < n; i++ {
		id := "trip-bulk-" + strconv.Itoa(i)
		trips.audience[id] = TripAudience{
			TripID: id, VehicleID: "veh-1", OwnerUserID: "owner",
			ParticipantUserIDs: []string{"p1"},
		}
		trips.toEnd = append(trips.toEnd, id)
	}

	if _, ended := NewSweeper(svc, nil).SweepOnce(context.Background()); ended != n {
		t.Fatalf("ended %d trips, want %d", ended, n)
	}
	svc.DrainRevalidation()

	got := reval.count()
	if got == 0 {
		t.Fatal("no sweep ran at all; a closed window would keep streaming live location")
	}
	if got > 1 {
		t.Errorf("%d fleet-wide re-mask sweeps for %d trip endings, want at most 1 "+
			"(one, after both loops) — the nudge is not coalescing", got, n)
	}
}
