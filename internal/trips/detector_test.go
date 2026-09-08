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

// The leg detector, driven by scripted frame sequences.
//
// Frames are built the way Tesla actually sends them — as DELTAS, each carrying
// only what changed — because that property is the whole reason this detector
// keeps a per-VIN cache instead of deciding from one frame, and a test that fed
// it complete frames would never exercise the design.

const (
	testVIN     = "5YJ3E1EA1PF000001"
	testVehicle = "veh-1"
	testTrip    = "trip-1"
)

var frameBase = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

type frame struct {
	afterSeconds int
	fields       map[string]events.TelemetryValue
}

func str(s string) *string   { return &s }
func f64(v float64) *float64 { return &v }
func at(sec int) time.Time   { return frameBase.Add(time.Duration(sec) * time.Second) }
func gear(g string) events.TelemetryValue {
	return events.TelemetryValue{StringVal: str(g)}
}
func speed(v float64) events.TelemetryValue {
	return events.TelemetryValue{FloatVal: f64(v)}
}
func dest(name string) events.TelemetryValue {
	return events.TelemetryValue{StringVal: str(name)}
}
func loc(lat, lng float64) events.TelemetryValue {
	return events.TelemetryValue{LocationVal: &events.Location{Latitude: lat, Longitude: lng}}
}

// newTestDetector wires a detector whose car is inside an open window.
// testDwell is the production 20 seconds, used by every case here on purpose:
// the dwell IS the defence against a false arrival, so a suite that shortened
// it would stop testing the thing most worth testing.
const testDwell = 20 * time.Second

func newTestDetector(t *testing.T) (
	*Detector, *fakeTripStore, *fakeLegStore, *fakePusher, *fakeActivityPusher,
) {
	t.Helper()
	trips, legs := newFakeTripStore(), newFakeLegStore()
	pusher, activities := &fakePusher{}, &fakeActivityPusher{}
	trips.vehicles = []TripVehicle{{VehicleID: testVehicle, TripID: testTrip}}
	trips.audience[testTrip] = TripAudience{
		TripID: testTrip, VehicleID: testVehicle, OwnerUserID: "owner",
		ParticipantUserIDs: []string{"p1"},
	}
	trips.names[testTrip] = "DFW to LA"

	svc := NewService(trips, legs, Config{Enabled: true, Dwell: testDwell}, nil).
		WithPushes(pusher).
		WithActivities(activities)
	svc.now = func() time.Time { return frameBase }

	d := NewDetector(svc, nil, &fakeVINResolver{byVIN: map[string]string{testVIN: testVehicle}}, nil)
	d.ctx = context.Background()
	return d, trips, legs, pusher, activities
}

// feed pushes a scripted sequence through the frame handler.
//
// THE SERVICE CLOCK FOLLOWS THE FRAMES. In production `now()` and a frame's
// timestamp are the same instant to within the streaming lag, and several rules
// — the leg-resume window most of all — compare a stamp taken from one against a
// deadline computed from the other. A frozen clock made those comparisons
// meaningless: a leg "ended" at second zero however far into the script it
// actually closed.
func feed(d *Detector, frames []frame) {
	for _, fr := range frames {
		now := at(fr.afterSeconds)
		d.svc.now = func() time.Time { return now }
		d.handleFrame(events.NewEvent(events.VehicleTelemetryEvent{
			VIN:       testVIN,
			CreatedAt: now,
			Fields:    fr.fields,
		}))
	}
}

// TestLegOpens covers BOTH transitions into "driving with a destination", and
// the second is the one no drive-start event could ever have expressed: the car
// was already moving and the driver set the route on the dash afterwards.
func TestLegOpens(t *testing.T) {
	tests := []struct {
		name   string
		frames []frame
	}{
		{
			name: "route set first, then the car pulls out",
			frames: []frame{
				{0, map[string]events.TelemetryValue{
					string(telemetry.FieldDestinationName): dest("Grand Canyon"),
					string(telemetry.FieldGear):            gear("P"),
				}},
				{5, map[string]events.TelemetryValue{
					string(telemetry.FieldGear):  gear("D"),
					string(telemetry.FieldSpeed): speed(12),
				}},
			},
		},
		{
			name: "car pulls out first, route set on the dash afterwards",
			frames: []frame{
				{0, map[string]events.TelemetryValue{
					string(telemetry.FieldGear):  gear("D"),
					string(telemetry.FieldSpeed): speed(20),
				}},
				{30, map[string]events.TelemetryValue{
					string(telemetry.FieldDestinationName): dest("Grand Canyon"),
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _, legs, pusher, activities := newTestDetector(t)
			feed(d, tt.frames)

			open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
			if len(open) != 1 {
				t.Fatalf("%d open legs, want 1", len(open))
			}
			if open[0].DestinationName != "Grand Canyon" {
				t.Errorf("destination = %q, want Grand Canyon", open[0].DestinationName)
			}
			if got := pusher.events(); len(got) != 1 || got[0] != push.TripEventLegStarted {
				t.Errorf("pushes = %v, want one trip_leg_started", got)
			}
			// EVERYONE, owner included: the owner is on the leg card by
			// explicit product decision, and a card with no banner would make
			// the driving party the one person never told anything.
			if len(pusher.sent[0].UserIDs) != 2 {
				t.Errorf("recipients = %v, want the participant AND the owner",
					pusher.sent[0].UserIDs)
			}
			if len(activities.starts) != 1 {
				t.Fatalf("push-to-started %d cards, want 1", len(activities.starts))
			}
			if got := activities.starts[0].TripName; got != "DFW to LA" {
				t.Errorf("card tripName = %q, want the trip's name — a participant may "+
					"hold cards for two shared cars at once", got)
			}
		})
	}
}

// TestLegDoesNotOpenWithoutADestination is the definition, stated as a test: a
// car pulling out of a driveway with no route set starts no leg. Access is
// unaffected either way — the window governs that — so this is purely about the
// card and the two leg pushes.
func TestLegDoesNotOpenWithoutADestination(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)
	feed(d, []frame{
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldGear):  gear("D"),
			string(telemetry.FieldSpeed): speed(35),
		}},
		{60, map[string]events.TelemetryValue{string(telemetry.FieldSpeed): speed(40)}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 0 {
		t.Errorf("%d legs opened for a destinationless drive", len(open))
	}
	if len(pusher.sent) != 0 || len(activities.starts) != 0 {
		t.Errorf("a destinationless drive produced %d pushes and %d cards",
			len(pusher.sent), len(activities.starts))
	}
}

// TestLegDoesNotReopenOnAReroute. A driver who changes the destination mid-leg
// has not started a second journey; a second leg would be a second card on
// every participant's lock screen for one drive.
func TestLegDoesNotReopenOnAReroute(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)
	feed(d, []frame{
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Grand Canyon"),
			string(telemetry.FieldGear):            gear("D"),
			string(telemetry.FieldSpeed):           speed(30),
		}},
		{120, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Sedona"),
		}},
	})

	if legs.startCalls == 0 {
		t.Fatal("no leg was ever started")
	}
	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 1 {
		t.Fatalf("%d open legs after a re-route, want 1", len(open))
	}
	if got := len(activities.starts); got != 1 {
		t.Errorf("push-to-started %d cards for one journey, want 1", got)
	}
	starts := 0
	for _, e := range pusher.events() {
		if e == push.TripEventLegStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("%d trip_leg_started pushes for one journey, want 1", starts)
	}
}

// TestLegArrivesOnDwellAtTheDestination is the happy path, and the dwell is the
// whole defence against a car that merely PASSES the destination.
func TestLegArrivesOnDwellAtTheDestination(t *testing.T) {
	d, _, legs, pusher, activities := newTestDetector(t)
	const destLat, destLng = 36.0544, -112.1401

	feed(d, []frame{
		// Under way, with the destination coordinate the car streams.
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Grand Canyon"),
			string(telemetry.FieldDestLocation):    loc(destLat, destLng),
			string(telemetry.FieldGear):            gear("D"),
			string(telemetry.FieldSpeed):           speed(55),
			string(telemetry.FieldLocation):        loc(36.20, -112.30),
		}},
		// Arrived and stopped — the dwell starts here.
		{600, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):    speed(0),
			string(telemetry.FieldLocation): loc(destLat, destLng),
		}},
		// Still there 25s later: past the 20s dwell.
		{625, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):    speed(0),
			string(telemetry.FieldLocation): loc(destLat, destLng),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 0 {
		t.Errorf("%d legs still open after arrival", len(open))
	}
	arrivals := 0
	for _, e := range pusher.events() {
		if e == push.TripEventLegArrived {
			arrivals++
		}
	}
	if arrivals != 1 {
		t.Errorf("%d trip_leg_arrived pushes, want exactly 1: %v", arrivals, pusher.events())
	}
	if len(activities.ends) != 1 || activities.ends[0].Status != tripStatusArrived {
		t.Errorf("final card = %v, want one ending at %q", activities.ends, tripStatusArrived)
	}
}

// TestLegDoesNotArriveOnAPassingStop is the case the dwell exists for: stopped
// at the lights outside the destination, then moving again.
func TestLegDoesNotArriveOnAPassingStop(t *testing.T) {
	d, _, legs, pusher, _ := newTestDetector(t)
	const destLat, destLng = 36.0544, -112.1401

	feed(d, []frame{
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Grand Canyon"),
			string(telemetry.FieldDestLocation):    loc(destLat, destLng),
			string(telemetry.FieldGear):            gear("D"),
			string(telemetry.FieldSpeed):           speed(40),
			string(telemetry.FieldLocation):        loc(36.20, -112.30),
		}},
		// At the destination and stopped — a red light.
		{600, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):    speed(0),
			string(telemetry.FieldLocation): loc(destLat, destLng),
		}},
		// Moving again 8 seconds later, well inside the dwell.
		{608, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):    speed(18),
			string(telemetry.FieldLocation): loc(destLat, destLng),
		}},
		{620, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):    speed(30),
			string(telemetry.FieldLocation): loc(36.10, -112.20),
		}},
	})

	open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
	if len(open) != 1 {
		t.Errorf("the leg was closed by a passing stop (%d open)", len(open))
	}
	for _, e := range pusher.events() {
		if e == push.TripEventLegArrived {
			t.Fatalf("an arrival fired for a car that stopped for 8 seconds: %v", pusher.events())
		}
	}
}

// TestLegCompletesWithoutEvidence covers the two endings that are NOT arrivals.
// The distinction is load-bearing: no `trip_leg_arrived`, and a final card that
// says the drive ended rather than that it arrived.
func TestLegCompletesWithoutEvidence(t *testing.T) {
	tests := []struct {
		name  string
		final []frame
	}{
		{
			// MYR-612: a cleared route now needs the clear to be CONFIRMED —
			// SUSTAINED past LegClearGrace with no arrival estimate — because
			// one delta carrying an empty name is not evidence the driver
			// cancelled anything. The second frame is what confirms it.
			name: "the driver cleared the route, and kept it cleared",
			final: []frame{
				{600, map[string]events.TelemetryValue{
					string(telemetry.FieldDestinationName): dest(""),
				}},
				{700, map[string]events.TelemetryValue{
					string(telemetry.FieldSpeed): speed(50),
				}},
			},
		},
		{
			name: "the car parked somewhere else",
			final: []frame{{600, map[string]events.TelemetryValue{
				string(telemetry.FieldGear):     gear("P"),
				string(telemetry.FieldLocation): loc(35.00, -111.00),
			}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _, legs, pusher, activities := newTestDetector(t)
			feed(d, append([]frame{
				{0, map[string]events.TelemetryValue{
					string(telemetry.FieldDestinationName): dest("Grand Canyon"),
					string(telemetry.FieldDestLocation):    loc(36.0544, -112.1401),
					string(telemetry.FieldGear):            gear("D"),
					string(telemetry.FieldSpeed):           speed(55),
					string(telemetry.FieldLocation):        loc(36.20, -112.30),
				}},
			}, tt.final...))

			open, _ := legs.OpenLegsForTrip(context.Background(), testTrip)
			if len(open) != 0 {
				t.Errorf("%d legs still open", len(open))
			}
			for _, e := range pusher.events() {
				if e == push.TripEventLegArrived {
					t.Errorf("an arrival push fired without evidence: %v", pusher.events())
				}
			}
			if len(activities.ends) != 1 {
				t.Fatalf("ended %d cards, want 1", len(activities.ends))
			}
			if got := activities.ends[0].Status; got != tripStatusCompleted {
				t.Errorf("final card status = %q, want %q", got, tripStatusCompleted)
			}
		})
	}
}

// TestDetectorIgnoresCarsOutsideAnyWindow: the overwhelmingly common frame. A
// car with no open trip must cost nothing and must leave no state behind.
func TestDetectorIgnoresCarsOutsideAnyWindow(t *testing.T) {
	d, trips, legs, pusher, _ := newTestDetector(t)
	trips.vehicles = nil

	feed(d, []frame{
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Grand Canyon"),
			string(telemetry.FieldGear):            gear("D"),
			string(telemetry.FieldSpeed):           speed(55),
		}},
	})

	if legs.startCalls != 0 {
		t.Errorf("StartLeg was called %d times for a car in no window", legs.startCalls)
	}
	if len(pusher.sent) != 0 {
		t.Errorf("a car in no window produced %d pushes", len(pusher.sent))
	}
	if len(d.vehicles) != 0 {
		t.Errorf("per-VIN state was retained for a car in no window: %v", d.vehicles)
	}
}

// TestCardUpdatesAreThrottled. A car streams up to once per second and Apple
// throttles Activity pushes by budget, so a refresh must earn its push twice
// over: the arrival minute has to move, and a floor interval has to pass.
func TestCardUpdatesAreThrottled(t *testing.T) {
	d, _, _, _, activities := newTestDetector(t)

	frames := []frame{{0, map[string]events.TelemetryValue{
		string(telemetry.FieldDestinationName):  dest("Grand Canyon"),
		string(telemetry.FieldGear):             gear("D"),
		string(telemetry.FieldSpeed):            speed(55),
		string(telemetry.FieldMinutesToArrival): speed(42),
	}}}
	// Thirty seconds of one-per-second frames whose ETA barely moves.
	for i := 1; i <= 30; i++ {
		minutes := 42.0
		if i > 20 {
			minutes = 41
		}
		frames = append(frames, frame{i, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed):            speed(55),
			string(telemetry.FieldMinutesToArrival): speed(minutes),
		}})
	}
	feed(d, frames)

	if got := len(activities.updates); got > 2 {
		t.Errorf("%d card updates from 31 frames in 30 seconds — Apple throttles this "+
			"surface by budget, and an unthrottled card would burn it in a minute", got)
	}
	if len(activities.updates) == 0 {
		t.Error("no card update at all; the ETA is the one number on the card that moves")
	}
}

// TestSecondLegToTheSameDestinationCanArrive is finding 5: a return journey.
//
// The arrival latch used to be cleared only on a destination NAME change, so a
// car that arrived at the Grand Canyon, parked, and later drove BACK to the
// Grand Canyon carried the latch into the second leg — whose arrival branch was
// therefore disabled for good. That leg could only ever end as `completed`, and
// "your car arrived" was never sent. The latch is now keyed on the leg id, so
// it cannot outlive the leg it is about whatever the previous one's ending did.
func TestSecondLegToTheSameDestinationCanArrive(t *testing.T) {
	d, _, _, pusher, _ := newTestDetector(t)

	const destName = "Grand Canyon Village"
	// destLat/destLng and the arrival fix are the same point, so the car is
	// unambiguously AT the destination on both legs.
	const dLat, dLng = 36.0544, -112.1401

	drive := func(base int) []frame {
		return []frame{
			{base, map[string]events.TelemetryValue{
				string(telemetry.FieldDestinationName): dest(destName),
				string(telemetry.FieldDestLocation):    loc(dLat, dLng),
				string(telemetry.FieldGear):            gear("D"),
				string(telemetry.FieldSpeed):           speed(45),
				string(telemetry.FieldLocation):        loc(36.20, -112.30),
			}},
			// Arrive and stand still for the whole dwell.
			{base + 60, map[string]events.TelemetryValue{
				string(telemetry.FieldSpeed):    speed(0),
				string(telemetry.FieldLocation): loc(dLat, dLng),
			}},
			{base + 60 + int(testDwell/time.Second) + 1, map[string]events.TelemetryValue{
				string(telemetry.FieldSpeed):    speed(0),
				string(telemetry.FieldLocation): loc(dLat, dLng),
			}},
		}
	}

	feed(d, drive(0))
	// The car sits for an hour, then drives back to the same named place.
	feed(d, drive(3600))

	var arrivals int
	for _, p := range pusher.sent {
		if p.Event == push.TripEventLegArrived {
			arrivals++
		}
	}
	if arrivals != 2 {
		t.Errorf("%d trip_leg_arrived pushes across two legs to the same destination, want 2 "+
			"— a latch that survives a leg disables every later arrival at that place",
			arrivals)
	}
}

// TestDestinationNameChangeInvalidatesTheCachedCoordinate is finding 4.
//
// Tesla streams deltas, so the frame announcing a NEW destination name usually
// carries no DestLocation. Left in place, the cached coordinate still points at
// the PREVIOUS destination while `destination` names the new one — and
// inRadius prefers the coordinate over the car's own milesToArrival, so the
// detector measured arrival against a place the car was no longer going.
func TestDestinationNameChangeInvalidatesTheCachedCoordinate(t *testing.T) {
	var v vehicleState
	cfg := Config{}.withDefaults()

	v.apply(fix{
		at:       at(0),
		destName: str("Home"),
		destLat:  36.0544, destLng: -112.1401, hasDest: true,
	}, cfg)
	if !v.hasDest {
		t.Fatal("the first destination's coordinate was not cached")
	}

	// A name change with NO coordinate, which is the ordinary delta shape.
	v.apply(fix{at: at(1), destName: str("Work")}, cfg)
	if v.hasDest {
		t.Fatal("the OLD destination's coordinate survived a change of destination; " +
			"arrival would be measured against a place the car is no longer going")
	}

	// With no coordinate, the car's own distance-to-arrival is what decides —
	// which on a leg is the same fact by construction (see arrivedAt).
	far := fix{at: at(2), lat: 36.0544, lng: -112.1401, hasFix: true, milesToGo: f64(40)}
	if v.inRadius(far, cfg) {
		t.Error("a car 40 miles from its NEW destination reported as arrived, because " +
			"it happened to be sitting on the old one's coordinate")
	}
	near := fix{at: at(3), milesToGo: f64(0.01)}
	if !v.inRadius(near, cfg) {
		t.Error("milesToArrival was ignored while no coordinate is known; the detector " +
			"has no other evidence at that moment")
	}

	// A fresh DestLocation re-arms the coordinate path in the same frame.
	v.apply(fix{at: at(4), destName: str("Work"), destLat: 1, destLng: 2, hasDest: true}, cfg)
	if !v.hasDest || v.destLat != 1 || v.destLng != 2 {
		t.Errorf("the new destination's coordinate was not adopted: %+v", v)
	}
}

// TestCandidateRefreshBacksOffOnFailure is finding 12.
//
// `ensure` stamped only successful refreshes, so after one failed read the
// snapshot was permanently older than the TTL and EVERY subsequent frame re-ran
// the query — a database blip turning a 15-second cache into a per-frame
// five-second-timeout read on the single bus goroutine, which drops frames for
// every other subscriber on the bus.
func TestCandidateRefreshBacksOffOnFailure(t *testing.T) {
	store := newFakeTripStore()
	store.vehiclErr = errors.New("connection refused")
	c := newLegCandidates(store, Config{Enabled: true}.withDefaults(), nil)

	now := frameBase
	for i := 0; i < 50; i++ {
		c.ensure(context.Background(), now.Add(time.Duration(i)*time.Second))
	}
	if store.vehicleCalls > 5 {
		t.Errorf("%d candidate reads across 50 seconds of frames with the database down; "+
			"the TTL is 15s, so at most a handful should have been attempted",
			store.vehicleCalls)
	}
	if store.vehicleCalls == 0 {
		t.Error("no read was attempted at all; the cache would never recover")
	}
}

// TestPruneDropsCarsThatLeftTheirWindow is W2: the per-car map must be bounded
// by the OPEN-WINDOW set, not by every car ever watched.
//
// It used to be cleared wholesale only when no window was open anywhere, and
// per-car only on a frame that car itself sent. A rolling fleet — windows
// always overlapping, cars going quiet when they park — therefore grew it
// monotonically.
func TestPruneDropsCarsThatLeftTheirWindow(t *testing.T) {
	d, trips, _, _, _ := newTestDetector(t)

	// A second car, in its own window, that will never send another frame.
	trips.vehicles = append(trips.vehicles, TripVehicle{VehicleID: "veh-gone", TripID: "trip-2"})
	d.vehicles["veh-gone"] = &vehicleState{destination: "Somewhere"}

	feed(d, []frame{{0, map[string]events.TelemetryValue{
		string(telemetry.FieldGear): gear("P"),
	}}})
	if _, held := d.vehicles["veh-gone"]; !held {
		t.Fatal("the seeded car was dropped before its window closed")
	}

	// Its window closes. The snapshot is rebuilt on the next frame past the
	// TTL, and the prune runs against it.
	trips.vehicles = []TripVehicle{{VehicleID: testVehicle, TripID: testTrip}}
	d.trips.attemptedAt = time.Time{}
	feed(d, []frame{{1, map[string]events.TelemetryValue{
		string(telemetry.FieldGear): gear("P"),
	}}})

	if _, held := d.vehicles["veh-gone"]; held {
		t.Error("a car outside every open window kept its state; with windows always " +
			"overlapping somewhere in the fleet, the map only ever grows")
	}
	if _, held := d.vehicles[testVehicle]; !held {
		t.Error("the prune dropped a car that is still inside its window")
	}
}

// TestLegIsNotOpenedOnAClosedWindow is the confirmation openLeg runs before it
// writes.
//
// The candidate snapshot is up to a TTL old and, on a failed refresh, four TTLs
// older than that. Opening a leg on a window that closed in the meantime pushes
// a Live Activity and a banner naming the car and its destination to people
// whose access was revoked with the window.
func TestLegIsNotOpenedOnAClosedWindow(t *testing.T) {
	d, trips, legs, pusher, activities := newTestDetector(t)

	// The snapshot still lists the car; the window itself has closed.
	d.trips.byVehicle = map[string]TripVehicle{testVehicle: {VehicleID: testVehicle, TripID: testTrip}}
	d.trips.attemptedAt = frameBase
	trips.vehicles = nil

	feed(d, []frame{
		{0, map[string]events.TelemetryValue{
			string(telemetry.FieldDestinationName): dest("Grand Canyon"),
			string(telemetry.FieldGear):            gear("P"),
		}},
		{5, map[string]events.TelemetryValue{
			string(telemetry.FieldGear):  gear("D"),
			string(telemetry.FieldSpeed): speed(30),
		}},
	})

	if legs.startCalls != 0 {
		t.Errorf("a leg was written on a window that has closed (%d StartLeg calls)", legs.startCalls)
	}
	if len(pusher.sent) != 0 || len(activities.starts) != 0 {
		t.Errorf("a closed window still pushed: %d banners, %d cards",
			len(pusher.sent), len(activities.starts))
	}
}
