package trips

import (
	"context"
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
func feed(d *Detector, frames []frame) {
	for _, fr := range frames {
		d.handleFrame(events.NewEvent(events.VehicleTelemetryEvent{
			VIN:       testVIN,
			CreatedAt: at(fr.afterSeconds),
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
		final frame
	}{
		{
			name: "the driver cleared the route",
			final: frame{600, map[string]events.TelemetryValue{
				string(telemetry.FieldDestinationName): dest(""),
			}},
		},
		{
			name: "the car parked somewhere else",
			final: frame{600, map[string]events.TelemetryValue{
				string(telemetry.FieldGear):     gear("P"),
				string(telemetry.FieldLocation): loc(35.00, -111.00),
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _, legs, pusher, activities := newTestDetector(t)
			feed(d, []frame{
				{0, map[string]events.TelemetryValue{
					string(telemetry.FieldDestinationName): dest("Grand Canyon"),
					string(telemetry.FieldDestLocation):    loc(36.0544, -112.1401),
					string(telemetry.FieldGear):            gear("D"),
					string(telemetry.FieldSpeed):           speed(55),
					string(telemetry.FieldLocation):        loc(36.20, -112.30),
				}},
				tt.final,
			})

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
