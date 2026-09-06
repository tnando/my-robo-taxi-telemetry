package push

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The trips fan-out (MYR-602). What is asserted here is everything the iOS lane
// and the preference gate depend on: the payload keys, the deep link, the
// per-leg collapse id, and that the `trips` switch actually silences it.

func newTripNotifier(t *testing.T, prefs Prefs) (*Notifier, *FakeSender) {
	t.Helper()
	sender := NewFakeSender()
	devices := newFakeDeviceStore()
	devices.byUser["user-a"] = []Device{{Token: "token-a", Sandbox: true}}
	devices.byUser["user-b"] = []Device{{Token: "token-b"}}
	prefStore := newFakePrefStore()
	prefStore.byUser["user-a"] = prefs
	prefStore.byUser["user-b"] = prefs
	n := NewNotifier(sender, devices, prefStore, nil, nil, Config{Enabled: true}, nil)
	return n, sender
}

func TestNotifyTrip_PayloadShape(t *testing.T) {
	tests := []struct {
		name            string
		push            TripPush
		wantEvent       string
		wantDestination string
		wantBodyHas     string
	}{
		{
			name: "added carries no destination",
			push: TripPush{
				TripID: "trip-1", VehicleID: "veh-1",
				Event: TripEventAdded, UserIDs: []string{"user-a"},
			},
			wantEvent:   "trip_added",
			wantBodyHas: "share its location",
		},
		{
			name: "leg started names the place",
			push: TripPush{
				TripID: "trip-1", VehicleID: "veh-1", LegID: "leg-9",
				Event: TripEventLegStarted, DestinationName: "Grand Canyon",
				UserIDs: []string{"user-a"},
			},
			wantEvent:       "trip_leg_started",
			wantDestination: "Grand Canyon",
			wantBodyHas:     "Heading to Grand Canyon",
		},
		{
			name: "leg arrived names the place",
			push: TripPush{
				TripID: "trip-1", VehicleID: "veh-1", LegID: "leg-9",
				Event: TripEventLegArrived, DestinationName: "Grand Canyon",
				UserIDs: []string{"user-a"},
			},
			wantEvent:       "trip_leg_arrived",
			wantDestination: "Grand Canyon",
			wantBodyHas:     "reached Grand Canyon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, sender := newTripNotifier(t, DefaultPrefs())
			n.NotifyTrip(context.Background(), tt.push)

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d notifications, want 1", len(sent))
			}
			got := sent[0]

			if got.RideID != "" {
				t.Errorf("RideID = %q, want empty — a trips push names no ride", got.RideID)
			}
			if got.UserInfo["tripId"] != tt.push.TripID {
				t.Errorf("userInfo.tripId = %q, want %q", got.UserInfo["tripId"], tt.push.TripID)
			}
			if got.UserInfo["vehicleId"] != tt.push.VehicleID {
				t.Errorf("userInfo.vehicleId = %q, want %q", got.UserInfo["vehicleId"], tt.push.VehicleID)
			}
			if got.UserInfo["event"] != tt.wantEvent {
				t.Errorf("userInfo.event = %q, want %q", got.UserInfo["event"], tt.wantEvent)
			}
			if want := "myrobotaxi://trips/" + tt.push.TripID; got.UserInfo["deepLink"] != want {
				t.Errorf("userInfo.deepLink = %q, want %q", got.UserInfo["deepLink"], want)
			}
			dest, present := got.UserInfo["destinationName"]
			if tt.wantDestination == "" {
				if present {
					t.Errorf("userInfo.destinationName = %q, want the key ABSENT on a "+
						"lifecycle event — an empty string is a value the client must special-case", dest)
				}
			} else if dest != tt.wantDestination {
				t.Errorf("userInfo.destinationName = %q, want %q", dest, tt.wantDestination)
			}
			if !strings.Contains(got.Body, tt.wantBodyHas) {
				t.Errorf("body = %q, want it to contain %q", got.Body, tt.wantBodyHas)
			}
		})
	}
}

// TestNotifyTrip_CollapseIsPerLeg is the reason TripPush carries a LegID it
// never puts on the wire: two consecutive legs of one trip must not merge their
// banners at Apple.
func TestNotifyTrip_CollapseIsPerLeg(t *testing.T) {
	n, sender := newTripNotifier(t, DefaultPrefs())
	base := TripPush{TripID: "trip-1", VehicleID: "veh-1", Event: TripEventLegStarted,
		DestinationName: "Somewhere", UserIDs: []string{"user-a"}}

	first := base
	first.LegID = "leg-1"
	second := base
	second.LegID = "leg-2"
	lifecycle := TripPush{TripID: "trip-1", VehicleID: "veh-1",
		Event: TripEventEnded, UserIDs: []string{"user-a"}}

	n.NotifyTrip(context.Background(), first)
	n.NotifyTrip(context.Background(), second)
	n.NotifyTrip(context.Background(), lifecycle)

	sent := sender.Sent()
	if len(sent) != 3 {
		t.Fatalf("sent %d notifications, want 3", len(sent))
	}
	if sent[0].CollapseSubject == sent[1].CollapseSubject {
		t.Errorf("two legs share collapse subject %q — Apple would merge their banners",
			sent[0].CollapseSubject)
	}
	if want := "trip-1|leg-1"; sent[0].CollapseSubject != want {
		t.Errorf("collapse subject = %q, want %q", sent[0].CollapseSubject, want)
	}
	if want := "trip-1"; sent[2].CollapseSubject != want {
		t.Errorf("lifecycle collapse subject = %q, want the bare trip %q",
			sent[2].CollapseSubject, want)
	}
}

// TestNotifyTrip_PreferenceGate proves the switch is real, and — the direction
// that actually matters — that it is the TRIPS switch and not a neighbour.
func TestNotifyTrip_PreferenceGate(t *testing.T) {
	tests := []struct {
		name     string
		prefs    Prefs
		wantSent int
	}{
		{"all on", DefaultPrefs(), 1},
		{"trips off", func() Prefs { p := DefaultPrefs(); p.Trips = false; return p }(), 0},
		{"ride lifecycle off does not silence trips",
			func() Prefs { p := DefaultPrefs(); p.RideLifecycle = false; return p }(), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, sender := newTripNotifier(t, tt.prefs)
			n.NotifyTrip(context.Background(), TripPush{
				TripID: "trip-1", VehicleID: "veh-1",
				Event: TripEventStarted, UserIDs: []string{"user-a"},
			})
			if got := len(sender.Sent()); got != tt.wantSent {
				t.Errorf("sent %d, want %d", got, tt.wantSent)
			}
		})
	}
}

// TestNotifyTrip_DeduplicatesRecipients covers the owner who is also listed as
// a participant: one fact, one buzz.
func TestNotifyTrip_DeduplicatesRecipients(t *testing.T) {
	n, sender := newTripNotifier(t, DefaultPrefs())
	n.NotifyTrip(context.Background(), TripPush{
		TripID: "trip-1", VehicleID: "veh-1", Event: TripEventStarted,
		UserIDs: []string{"user-a", "", "user-a", "user-b"},
	})
	if got := len(sender.Sent()); got != 2 {
		t.Errorf("sent %d notifications, want 2 (one per distinct recipient)", got)
	}
}

// TestTripPayload_OmitsRideID pins the wire shape at the JSON level: a trips
// push must not carry `rideId: ""`, and a ride push must be unchanged.
func TestTripPayload_OmitsRideID(t *testing.T) {
	tripBody, err := buildPayload(Notification{
		Title: "t", Body: "b",
		UserInfo: TripPush{TripID: "trip-1", VehicleID: "veh-1", Event: TripEventAdded}.userInfo(),
	})
	if err != nil {
		t.Fatalf("buildPayload(trip): %v", err)
	}
	var trip map[string]any
	if err := json.Unmarshal(tripBody, &trip); err != nil {
		t.Fatalf("unmarshal trip payload: %v", err)
	}
	if _, present := trip["rideId"]; present {
		t.Errorf("trips payload carries rideId: %v", trip["rideId"])
	}
	if trip["tripId"] != "trip-1" {
		t.Errorf("tripId = %v, want trip-1", trip["tripId"])
	}

	rideBody, err := buildPayload(Notification{Title: "t", Body: "b", RideID: "ride-1"})
	if err != nil {
		t.Fatalf("buildPayload(ride): %v", err)
	}
	var ride map[string]any
	if err := json.Unmarshal(rideBody, &ride); err != nil {
		t.Fatalf("unmarshal ride payload: %v", err)
	}
	if ride["rideId"] != "ride-1" {
		t.Errorf("ride payload lost rideId: %v", ride)
	}
}
