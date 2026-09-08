package trips

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/push"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// MYR-612 — THE FLAP, REPRODUCED FROM THE RECORDED FRAME SEQUENCE.
//
// Prod, 2026-09-08, a car four minutes into a leg to "Element by Marriott
// Sedona". `go_trip_legs` recorded leg A 03:36:31 → 03:40:22 (arrived=false)
// and leg B opened 03:40:24, still driving to the same hotel. The Vehicle row
// at 03:41:58 read `status=driving, destinationName=NULL, etaMinutes=98` — the
// dash never stopped showing the place; ONE DELTA carried an empty name.
//
// The shape is: driving + name present → driving + name ABSENT + ETA present →
// driving + name present. Each of the tests below drives exactly that.

const sedona = "Element by Marriott Sedona"

// underway is the opening frame of the incident's leg.
func underway() frame {
	return frame{0, map[string]events.TelemetryValue{
		string(telemetry.FieldDestinationName):  dest(sedona),
		string(telemetry.FieldGear):             gear("D"),
		string(telemetry.FieldSpeed):            speed(62),
		string(telemetry.FieldMinutesToArrival): speed(99),
	}}
}

// TestTransientDestinationClearDoesNotCloseTheLeg is the headline: the exact
// three-frame sequence must leave ONE leg open, with one start banner and one
// push-to-start.
func TestTransientDestinationClearDoesNotCloseTheLeg(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)

	feed(d, []frame{
		underway(),
		// The delta that broke it: an empty name, the car still driving, the
		// arrival estimate still flowing.
		{230, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName):  dest(""),
			string(telemetry.FieldSpeed):            speed(60),
			string(telemetry.FieldMinutesToArrival): speed(98),
		}},
		// Two seconds later the name is back, exactly as in production.
		{232, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName):  dest(sedona),
			string(telemetry.FieldSpeed):            speed(60),
			string(telemetry.FieldMinutesToArrival): speed(98),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 1 {
		t.Fatalf("open legs = %d, want 1 (the transient clear must not close the leg)", len(open))
	}
	if got := len(legs.byID); got != 1 {
		t.Errorf("legs recorded = %d, want 1 — the journey flapped into two rows", got)
	}
	if got := starts(pusher); got != 1 {
		t.Errorf("trip_leg_started banners = %d, want 1", got)
	}
	if got := len(activities.starts); got != 1 {
		t.Errorf("push-to-start fan-outs = %d, want 1", got)
	}
	if got := len(activities.ends); got != 0 {
		t.Errorf("cards ended = %d, want 0 — leg A's card was ended while the car drove on", got)
	}
}

// TestSustainedDestinationClearStillClosesTheLeg is the other direction, and it
// is the one the debounce must not break: a driver who really did cancel
// navigation still gets the leg closed, once the clear has outlived the grace
// AND no arrival estimate has been reported in that time.
func TestSustainedDestinationClearStillClosesTheLeg(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)
	grace := int(d.cfg.LegClearGrace.Seconds())

	feed(d, []frame{
		underway(),
		{230, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldSpeed):           speed(60),
		}},
		// Still driving, still no route, and no estimate since the clear.
		{230 + grace + 1, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): speed(60),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 0 {
		t.Fatalf("open legs = %d, want 0 — a real cancellation must still end the leg", len(open))
	}
	for _, e := range pusher.events() {
		if e == push.TripEventLegArrived {
			t.Fatalf("a cleared route is not an arrival: %v", pusher.events())
		}
	}
	if len(activities.ends) != 1 {
		t.Fatalf("cards ended = %d, want 1", len(activities.ends))
	}
	if got := activities.ends[0].Status; got != tripStatusCompleted {
		t.Errorf("final card status = %q, want %q", got, tripStatusCompleted)
	}
}

// TestAnEstimateHoldsTheLegOpenPastTheGrace pins the second half of the
// confirmation rule. A car still saying how long it has to go still has
// somewhere to be, whatever a delta left out.
func TestAnEstimateHoldsTheLegOpenPastTheGrace(t *testing.T) {
	d, _, legs, _, _ := newTestDetector(t)
	grace := int(d.cfg.LegClearGrace.Seconds())

	frames := []frame{underway()}
	// Ten minutes of name-less frames that still carry an estimate.
	for sec := 230; sec <= 230+10*grace; sec += 10 {
		frames = append(frames, frame{sec, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName):  dest(""),
			string(telemetry.FieldSpeed):            speed(60),
			string(telemetry.FieldMinutesToArrival): speed(40),
		}})
	}
	feed(d, frames)

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 1 {
		t.Fatalf("open legs = %d, want 1 — an estimate is evidence the route is intact", len(open))
	}
}

// TestParkEndsALegWithoutWaitingForTheGrace: the debounce delays a verdict it
// is unsure of, and a parked car with no route is not one of those.
func TestParkEndsALegWithoutWaitingForTheGrace(t *testing.T) {
	d, _, legs, _, _ := newTestDetector(t)

	feed(d, []frame{
		underway(),
		{230, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldGear):            gear("P"),
			string(telemetry.FieldSpeed):           speed(0),
			string(telemetry.FieldLocation):        loc(35.00, -111.00),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 0 {
		t.Fatalf("open legs = %d, want 0 — a parked car with no route needs no debounce", len(open))
	}
}

// TestAReopenedJourneyResumesItsLeg covers the second line of defence: a leg
// that closed anyway — a restart between the frames, a rolling deploy, a grace
// that expired one frame early — is RESUMED rather than duplicated when the car
// sets off again for the same place.
func TestAReopenedJourneyResumesItsLeg(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)
	grace := int(d.cfg.LegClearGrace.Seconds())

	feed(d, []frame{
		underway(),
		{230, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldSpeed):           speed(60),
		}},
		{230 + grace + 1, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): speed(60),
		}},
	})
	if open, _ := legs.OpenLegsForTrip(context.Background(), testTrip); len(open) != 0 {
		t.Fatalf("setup: the leg should have closed, %d open", len(open))
	}
	legID := onlyLegID(t, legs)

	// The name comes back a few seconds later — the same journey all along.
	feed(d, []frame{{230 + grace + 6, map[string]events.TelemetryValue{
		string(telemetry.FieldDestinationName): dest(sedona),
		string(telemetry.FieldSpeed):           speed(60),
	}}})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 1 {
		t.Fatalf("open legs = %d, want 1", len(open))
	}
	if open[0].ID != legID {
		t.Errorf("resumed leg = %q, want the original %q — the journey was recorded twice", open[0].ID, legID)
	}
	if got := len(legs.byID); got != 1 {
		t.Errorf("legs recorded = %d, want 1", got)
	}
	// The banner is NOT repeated: its claim survives a resume.
	if got := starts(pusher); got != 1 {
		t.Errorf("trip_leg_started banners = %d, want 1", got)
	}
	// The card WAS ended, so a new one is raised — that is the state the lock
	// screen is actually in.
	if got := len(activities.starts); got != 2 {
		t.Errorf("push-to-start fan-outs = %d, want 2 (the first card was ended)", got)
	}
}

// TestADifferentDestinationStartsANewLeg: the merge is about ONE journey, and a
// car that sets off somewhere else has made a second.
func TestADifferentDestinationStartsANewLeg(t *testing.T) {
	d, _, legs, _, _ := newTestDetector(t)
	grace := int(d.cfg.LegClearGrace.Seconds())

	feed(d, []frame{
		underway(),
		{230, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldSpeed):           speed(60),
		}},
		{230 + grace + 1, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): speed(60),
		}},
		{230 + grace + 6, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Grand Canyon"),
			string(telemetry.FieldSpeed):           speed(60),
		}},
	})

	if got := len(legs.byID); got != 2 {
		t.Errorf("legs recorded = %d, want 2 — a different destination is a different journey", got)
	}
}

// TestAnArrivedLegIsNeverResumed: "your car arrived" cannot be taken back.
func TestAnArrivedLegIsNeverResumed(t *testing.T) {
	legs := newFakeLegStore()
	end := frameBase.Add(time.Minute)
	legs.byID["leg-1"] = &Leg{
		ID: "leg-1", TripID: testTrip, VehicleID: testVehicle,
		DestinationName: sedona, StartedAt: frameBase, EndedAt: &end,
	}
	legs.arrived["leg-1"] = true

	_, resumed, err := legs.ResumeRecentLeg(
		context.Background(), testVehicle, sedona, frameBase)
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Error("an arrived leg was resumed; the arrival push has already gone out")
	}
}

// starts counts trip_leg_started banners.
func starts(p *fakePusher) int {
	n := 0
	for _, e := range p.events() {
		if e == push.TripEventLegStarted {
			n++
		}
	}
	return n
}

func onlyLegID(t *testing.T, legs *fakeLegStore) string {
	t.Helper()
	if len(legs.byID) != 1 {
		t.Fatalf("expected exactly one leg, got %d", len(legs.byID))
	}
	for id := range legs.byID {
		return id
	}
	return ""
}

// TestARouteClearedWhileParkedOpensNoPhantomLeg is the pending clear's other
// half, and the one the debounce got wrong (MYR-612 review).
//
// A clear is PENDING until something confirms it, and the only confirmation
// path ran inside `decide`'s leg-open branch. A driver who arrives, parks, and
// THEN cancels the route therefore left the pending clear in memory for ever:
// `destination` went on naming a place nobody was going, and the next time the
// car pulled out — with no route set at all — it read as "driving with a
// destination" and opened a leg, with a banner and a card, for a journey that
// did not exist.
func TestARouteClearedWhileParkedOpensNoPhantomLeg(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)

	feed(d, []frame{
		// Arrive: the car reaches the hotel and sits there for the dwell.
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(sedona),
			string(telemetry.FieldSpeed):           speed(40),
			string(telemetry.FieldMilesToArrival):  speed(4),
		}},
		{60, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):          speed(0),
			string(telemetry.FieldMilesToArrival): speed(0.01),
		}},
		{90, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):          speed(0),
			string(telemetry.FieldMilesToArrival): speed(0.01),
		}},
	})
	if open, _ := legs.OpenLegsForTrip(context.Background(), testTrip); len(open) != 0 {
		t.Fatalf("setup: the leg should have arrived and closed, %d open", len(open))
	}

	feed(d, []frame{
		// The driver clears the route on the dash while parked. No leg is
		// open, so nothing in the leg path can confirm the clear.
		{120, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldGear):            gear("P"),
		}},
		// The next day the car sets off with NO destination at all.
		{9000, map[string]events.TelemetryValue{
			string(telemetry.FieldGear):  gear("D"),
			string(telemetry.FieldSpeed): speed(35),
		}},
	})

	if got := len(legs.byID); got != 1 {
		t.Errorf("legs recorded = %d, want 1 — a phantom leg opened with no route set", got)
	}
	if got := starts(pusher); got != 1 {
		t.Errorf("trip_leg_started banners = %d, want 1", got)
	}
	if got := len(activities.starts); got != 1 {
		t.Errorf("push-to-start fan-outs = %d, want 1", got)
	}
}

// TestAClearArrivingWithItsOwnEstimateStillCloses is the incident's OWN frame
// shape, run forward to the ending it was supposed to reach (MYR-612 review).
//
// Tesla sent the empty name and `minutesToArrival` in ONE frame, so the clear
// stamp and the estimate stamp were the same instant. The confirmation rule
// asked whether the last estimate came STRICTLY BEFORE the clear, which for
// equal stamps is false — and false for ever after, since any later frame
// carrying an estimate pushed the estimate stamp past the clear. A route the
// driver really did cancel could then never close its leg: the card sat on the
// lock screen until the window itself lapsed.
//
// The rule is staleness, and staleness alone: no estimate for a whole grace.
func TestAClearArrivingWithItsOwnEstimateStillCloses(t *testing.T) {
	d, _, legs, _, activities := newTestDetector(t)
	grace := int(d.cfg.LegClearGrace.Seconds())

	feed(d, []frame{
		underway(),
		// The incident's frame: both facts in one delta, same timestamp.
		{230, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName):  dest(""),
			string(telemetry.FieldSpeed):            speed(60),
			string(telemetry.FieldMinutesToArrival): speed(98),
		}},
		// Then silence on both — the shape of a real cancellation.
		{230 + grace + 1, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): speed(60),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 0 {
		t.Fatalf("open legs = %d, want 0 — a cancelled route must still close its leg", len(open))
	}
	if len(activities.ends) != 1 {
		t.Fatalf("cards ended = %d, want 1 — the card outlived the route", len(activities.ends))
	}
	if got := activities.ends[0].Status; got != tripStatusCompleted {
		t.Errorf("final card status = %q, want %q", got, tripStatusCompleted)
	}
}

// TestARedLightDoesNotConfirmAClear — the debounce must survive a junction
// (MYR-612 review).
//
// The park rung of the confirmation read `!driving`, which is true of any
// frame reporting a speed at or under 1 mph. A car that waits at a light for
// eight seconds reports exactly that, so a delta which happened to omit the
// destination name followed by ONE red-light frame closed the leg on the spot
// — the sixty-second grace bought nothing at all on a city street.
func TestARedLightDoesNotConfirmAClear(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)

	feed(d, []frame{
		underway(),
		// The delta with no name, still moving.
		{230, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName):  dest(""),
			string(telemetry.FieldSpeed):            speed(28),
			string(telemetry.FieldMinutesToArrival): speed(12),
		}},
		// A junction: stopped, well short of the destination, for 8 seconds.
		{235, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): speed(0),
		}},
		{240, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): speed(0),
		}},
		// Green, and the name is back in the next delta.
		{243, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName):  dest(sedona),
			string(telemetry.FieldSpeed):            speed(24),
			string(telemetry.FieldMinutesToArrival): speed(11),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 1 {
		t.Fatalf("open legs = %d, want 1 — a red light is not a park", len(open))
	}
	if got := len(legs.byID); got != 1 {
		t.Errorf("legs recorded = %d, want 1", got)
	}
	if got := starts(pusher); got != 1 {
		t.Errorf("trip_leg_started banners = %d, want 1", got)
	}
	if got := len(activities.ends); got != 0 {
		t.Errorf("cards ended = %d, want 0", got)
	}
}

// TestAClearOnArrivalStillArrives — the `!atDestination` guard, carried from
// decide's park-short branch onto the clear confirmation (MYR-612 review).
//
// A dash clears its route when it reaches the place, so the arrival frame and
// the clear frame are frequently the same frame. Without the guard the park
// rung settled the clear one second into a twenty-second dwell and the leg
// closed as `completed`: "your car arrived" was never sent for the one ending
// that had earned it.
func TestAClearOnArrivalStillArrives(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)

	feed(d, []frame{
		// Under way, measuring the distance the dash reports.
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(sedona),
			string(telemetry.FieldGear):            gear("D"),
			string(telemetry.FieldSpeed):           speed(30),
			string(telemetry.FieldMilesToArrival):  speed(2),
		}},
		// Arrival: stopped at the destination AND the route cleared, in the
		// one frame the dash actually sends.
		{100, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldGear):            gear("P"),
			string(telemetry.FieldSpeed):           speed(0),
			string(telemetry.FieldMilesToArrival):  speed(0.01),
		}},
		// The dwell plays out on the REST-poll cadence.
		{110, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):          speed(0),
			string(telemetry.FieldMilesToArrival): speed(0.01),
		}},
		{125, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):          speed(0),
			string(telemetry.FieldMilesToArrival): speed(0.01),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 0 {
		t.Fatalf("open legs = %d, want 0 — the car arrived", len(open))
	}
	var arrived bool
	for _, e := range pusher.events() {
		if e == push.TripEventLegArrived {
			arrived = true
		}
	}
	if !arrived {
		t.Errorf("no trip_leg_arrived push: %v — the clear closed the leg inside the dwell", pusher.events())
	}
	if len(activities.ends) != 1 {
		t.Fatalf("cards ended = %d, want 1", len(activities.ends))
	}
	if got := activities.ends[0].Status; got != tripStatusArrived {
		t.Errorf("final card status = %q, want %q", got, tripStatusArrived)
	}
}
