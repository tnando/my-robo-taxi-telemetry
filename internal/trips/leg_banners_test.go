package trips

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/push"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// MYR-620 — TEN BANNERS IN AN HOUR.
//
// The client's lock screen, 2026-09-08: ten "Tesla is on the move — Heading to
// Element by Marriott Sedona." banners in 59 minutes, five of them inside a
// single minute. Every one was a correctly-claimed, once-per-leg
// `trip_leg_started` push — the LEG flapped, and `started_notified_at` is a
// stamp on a ROW that each reopen replaced.
//
// MYR-612's debounce and resume make that flap far rarer; these tests pin the
// property that holds whatever the detector does.

// flapping scripts the recorded incident shape N times over: driving with a
// name, one delta with the name absent long enough to close the leg, then the
// name back. Each cycle opened a leg before MYR-612 and can still open one
// after it, on the far side of a restart or a rolling deploy.
func flapping(d *Detector, cycles int) {
	grace := int(d.cfg.LegClearGrace.Seconds())
	sec := 0
	frames := []frame{underway()}
	for i := 0; i < cycles; i++ {
		sec += 30
		frames = append(frames, frame{sec, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldSpeed):           speed(60),
		}})
		sec += grace + 1
		frames = append(frames, frame{sec, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): speed(60),
		}})
		sec += 5
		frames = append(frames, frame{sec, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(sedona),
			string(telemetry.FieldSpeed):           speed(60),
		}})
	}
	feed(d, frames)
}

// TestAFlappingLegAnnouncesItselfOnce is the headline.
//
// The resume probe is made to FAIL, deliberately, because that is the state
// this gate is the backstop for: MYR-612's merge already collapses the ordinary
// flap into one row, and what it cannot cover is a restart between the two
// frames, two servers during a rolling deploy, or a store that could not answer
// — the very conditions under which the leg really does reopen as a new row and
// claims its own `started_notified_at`.
func TestAFlappingLegAnnouncesItselfOnce(t *testing.T) {
	d, _, legs, pusher, _ := newTestDetector(t)
	legs.resumeErr = errors.New("the merge probe could not be answered")

	flapping(d, 6)

	// The flap really did produce more than one leg — otherwise this test
	// would pass for the wrong reason.
	if len(legs.byID) < 2 {
		t.Fatalf("legs recorded = %d; the script no longer reproduces the flap", len(legs.byID))
	}
	if got := starts(pusher); got != 1 {
		t.Errorf("trip_leg_started banners = %d, want 1 — the client's screenshot "+
			"showed ten of these for one journey", got)
	}
}

// TestTheBannerWindowIsPerDestination: a car that really does set off for
// somewhere ELSE is announced, immediately, whatever the last banner said.
func TestTheBannerWindowIsPerDestination(t *testing.T) {
	d, _, _, pusher, _ := newTestDetector(t)
	grace := int(d.cfg.LegClearGrace.Seconds())

	feed(d, []frame{
		underway(),
		// Arrives, parks, and the leg closes.
		{200, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest(""),
			string(telemetry.FieldGear):            gear("P"),
			string(telemetry.FieldSpeed):           speed(0),
			string(telemetry.FieldLocation):        loc(35.00, -111.00),
		}},
		// A new destination, minutes later.
		{200 + grace + 10, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Grand Canyon Village"),
			string(telemetry.FieldSpeed):           speed(45),
		}},
	})

	if got := starts(pusher); got != 2 {
		t.Errorf("banners = %d, want 2 — a different place is different news", got)
	}
}

// TestTheBannerWindowReopensAfterItElapses: the suppression is a window and not
// a latch. A driver who arrives, waits an afternoon and drives back to the same
// hotel is told about the second journey.
func TestTheBannerWindowReopensAfterItElapses(t *testing.T) {
	legs := newFakeLegStore()
	svc := NewService(newFakeTripStore(), legs, Config{Enabled: true}, nil)
	leg := Leg{ID: "leg-1", TripID: testTrip, VehicleID: testVehicle, DestinationName: sedona}
	ctx := context.Background()

	base := time.Now().UTC()
	svc.now = func() time.Time { return base }
	if !svc.legBannerAllowed(ctx, leg, push.TripEventLegStarted) {
		t.Fatal("the first banner was suppressed")
	}
	svc.now = func() time.Time { return base.Add(svc.cfg.LegBannerWindow - time.Minute) }
	if svc.legBannerAllowed(ctx, leg, push.TripEventLegStarted) {
		t.Error("a repeat inside the window was allowed")
	}
	svc.now = func() time.Time { return base.Add(svc.cfg.LegBannerWindow + time.Minute) }
	if !svc.legBannerAllowed(ctx, leg, push.TripEventLegStarted) {
		t.Error("the window never reopens; a genuine second journey is never announced")
	}
}

// TestDepartureAndArrivalHaveTheirOwnSlots: they are different sentences about
// the same journey, and the second reports the outcome.
func TestDepartureAndArrivalHaveTheirOwnSlots(t *testing.T) {
	legs := newFakeLegStore()
	svc := NewService(newFakeTripStore(), legs, Config{Enabled: true}, nil)
	leg := Leg{ID: "leg-1", TripID: testTrip, VehicleID: testVehicle, DestinationName: sedona}
	ctx := context.Background()

	if !svc.legBannerAllowed(ctx, leg, push.TripEventLegStarted) {
		t.Fatal("the departure was suppressed")
	}
	if !svc.legBannerAllowed(ctx, leg, push.TripEventLegArrived) {
		t.Error("the arrival was suppressed by the departure; it reports the outcome")
	}
}

// TestTheBannerWindowFailsOpen: a store error sends the banner. A duplicate is
// an annoyance; a silence is somebody never told their car set off.
func TestTheBannerWindowFailsOpen(t *testing.T) {
	legs := newFakeLegStore()
	legs.bannerErr = errors.New("pool timeout")
	svc := NewService(newFakeTripStore(), legs, Config{Enabled: true}, nil)
	leg := Leg{ID: "leg-1", TripID: testTrip, VehicleID: testVehicle, DestinationName: sedona}

	if !svc.legBannerAllowed(context.Background(), leg, push.TripEventLegStarted) {
		t.Error("a database hiccup silenced a departure")
	}
}

// TestTheDestinationKeyIsADigestOfANormalisedName.
//
// A destination is P1, so the suppression table holds a digest and never the
// name. Normalising first is what stops the rule being defeated by a space:
// Tesla re-sends the name on every re-route and neither the casing nor the
// inner whitespace is stable across those.
func TestTheDestinationKeyIsADigestOfANormalisedName(t *testing.T) {
	base := destinationKey(sedona)
	if base == "" || base == sedona {
		t.Fatalf("key = %q; it must be a digest, never the P1 name", base)
	}
	for _, variant := range []string{
		"element by marriott sedona",
		"  Element  by   Marriott Sedona  ",
		"ELEMENT BY MARRIOTT SEDONA",
	} {
		if got := destinationKey(variant); got != base {
			t.Errorf("destinationKey(%q) = %q, want the same slot as %q", variant, got, sedona)
		}
	}
	if destinationKey("Grand Canyon Village") == base {
		t.Error("two different places share a slot")
	}
	if destinationKey("   ") != "" {
		t.Error("a nameless leg must not share one slot with every other nameless leg")
	}
}
