package trips

import (
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// MYR-612 — WHAT ONE FRAME COSTS IN QUERIES.
//
// The incident's second-order damage came from here. A car inside an open
// window with a leg underway ran TWO unbounded statements per frame on the
// single bus goroutine — the open-leg read and the trip audience — at up to one
// frame per second, for the whole four minutes of the journey. That is how a
// JWT existence probe belonging to an unrelated HTTP request came to time out
// and answer 401 (internal/auth/user_existence_cache.go).
//
// These are COST assertions, which is unusual for a unit test and deliberate:
// the behaviour they protect is invisible in every functional test, and it is
// the property that regressed.

// oneSecondApart scripts n frames of an under-way leg, one second apart,
// carrying the deltas a real car sends between edges: a speed, and nothing else
// that could change a decision.
func driveOn(d *Detector, fromSecond, n int) {
	for i := 0; i < n; i++ {
		d.handleFrame(events.NewEvent(events.VehicleTelemetryEvent{
			VIN:       testVIN,
			CreatedAt: at(fromSecond + i),
			Fields: map[string]events.TelemetryValue{
				string(telemetry.FieldSpeed): speed(40),
			},
		}))
	}
}

func TestUnderwayFramesDoNotQueryPerFrame(t *testing.T) {
	d, trips, legs, _, _ := newTestDetector(t)

	// Open a leg: driving, with a destination.
	feed(d, []frame{{0, map[string]events.TelemetryValue{
		string(telemetry.FieldDestinationName): dest("Element by Marriott Sedona"),
		string(telemetry.FieldSpeed):           speed(35),
	}}})
	if len(legs.openByVeh) != 1 {
		t.Fatalf("expected an open leg, got %d", len(legs.openByVeh))
	}

	trips.mu.Lock()
	audienceBefore := trips.audienceCalls
	trips.mu.Unlock()
	legs.mu.Lock()
	openBefore := legs.openCalls
	legs.mu.Unlock()

	// Sixty seconds of ordinary driving. Nothing about the leg changes.
	const frames = 60
	driveOn(d, 1, frames)

	trips.mu.Lock()
	audience := trips.audienceCalls - audienceBefore
	trips.mu.Unlock()
	legs.mu.Lock()
	opens := legs.openCalls - openBefore
	legs.mu.Unlock()

	// THE AUDIENCE IS NOT READ AT ALL while a leg simply continues. It is read
	// at an EDGE — the one outcome that uses it.
	if audience != 0 {
		t.Errorf("audience reads while merely under way = %d, want 0 "+
			"(this read belongs at the leg edge, not on the frame path)", audience)
	}

	// The open-leg read is served from memory for LegReadTTL. At one frame per
	// second over 60 seconds with the default 5-second TTL that is about a
	// dozen reads, not sixty; the assertion is deliberately loose about the
	// exact number and strict about the ORDER OF MAGNITUDE.
	if opens >= frames {
		t.Errorf("open-leg reads = %d over %d frames; the LegReadTTL cache is not working", opens, frames)
	}
	if want := frames / int(d.cfg.LegReadTTL.Seconds()); opens > want+2 {
		t.Errorf("open-leg reads = %d over %d frames, want about %d (one per %s)",
			opens, frames, want, d.cfg.LegReadTTL)
	}
}

// TestALegEdgeStillReadsTheAudience is the other half: thinning the frame path
// must not have made an edge silent.
func TestALegEdgeStillReadsTheAudience(t *testing.T) {
	d, trips, _, pusher, _ := newTestDetector(t)

	feed(d, []frame{
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Element by Marriott Sedona"),
			string(telemetry.FieldSpeed):           speed(35),
		}},
		// Parked short, well away from the destination: a `completed` ending.
		{600, map[string]events.TelemetryValue{
			string(telemetry.FieldGear):           gear("P"),
			string(telemetry.FieldSpeed):          speed(0),
			string(telemetry.FieldMilesToArrival): f64Value(12),
		}},
	})

	trips.mu.Lock()
	calls := trips.audienceCalls
	trips.mu.Unlock()
	if calls == 0 {
		t.Fatal("a leg edge must still resolve the audience")
	}
	if len(pusher.sent) == 0 {
		t.Fatal("the leg-start banner must still have gone out")
	}
}

func f64Value(v float64) events.TelemetryValue {
	return events.TelemetryValue{FloatVal: f64(v)}
}

// TestEveryFrameIsBounded — MYR-612 review.
//
// The budget used to be applied at each store call, and the VIN→vehicle
// resolution had none: on a cache miss it was an UNBOUNDED query on the single
// goroutine the bus delivers every subscriber's frames on. Bounding it at each
// call site is a rule the next call site can forget, so the deadline is set
// once, at the entrance, and this asserts the frame path actually carries it.
func TestEveryFrameIsBounded(t *testing.T) {
	d, _, _, _, _ := newTestDetector(t)
	vins, ok := d.vins.(*fakeVINResolver)
	if !ok {
		t.Fatalf("unexpected resolver %T", d.vins)
	}

	feed(d, []frame{{0, map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed): speed(35),
	}}})

	if !vins.sawCtx {
		t.Fatal("the frame never reached the VIN resolution")
	}
	if !vins.deadline {
		t.Fatal("the VIN resolution ran with no deadline; on a cache miss that is " +
			"an unbounded query on the bus delivery goroutine")
	}
	if vins.remaining <= 0 || vins.remaining > d.cfg.FrameTimeout {
		t.Errorf("remaining budget = %v, want (0, %v] — the frame's own ceiling",
			vins.remaining, d.cfg.FrameTimeout)
	}
}
