package push

import (
	"encoding/json"
	"testing"
	"time"
)

// THE PUSH-TO-START WIRE SHAPE (MYR-602). These assertions are the contract the
// iOS lane compiles against: `attributes-type` must be the widget's struct name
// EXACTLY, and every key here is one iOS matches by name. A mismatch fails
// SILENTLY — APNs answers 200, the device drops the push, no card appears, and
// there is no signal on either side — which is why this is pinned at the JSON
// level rather than at the Go struct.

func decodeAPS(t *testing.T, n ActivityNotification) map[string]any {
	t.Helper()
	raw, err := buildActivityPayload(n)
	if err != nil {
		t.Fatalf("buildActivityPayload: %v", err)
	}
	var payload struct {
		APS map[string]any `json:"aps"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload.APS
}

func tripLegFixture() TripLegContext {
	eta := 23
	return TripLegContext{
		LegID:       "leg-1",
		TripID:      "trip-1",
		VehicleID:   "veh-1",
		TripName:    "DFW to LA",
		VehicleName: "Optimus",
		Destination: "Grand Canyon Village",
		Status:      tripStatusEnroute,
		ETAMinutes:  &eta,
	}
}

func TestPushToStartPayload(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tc := tripLegFixture()

	aps := decodeAPS(t, ActivityNotification{
		ActivityToken: "pts-token",
		Event:         ActivityEventStart,
		ContentState:  tripContentState(tc, now),
		Timestamp:     now,
		Start: &TripActivityStart{
			TripID: tc.TripID, VehicleID: tc.VehicleID,
			LegID: tc.LegID, VehicleName: tc.VehicleName,
		},
	})

	if aps["event"] != "start" {
		t.Errorf("aps.event = %v, want start", aps["event"])
	}
	if aps["attributes-type"] != "TripActivityAttributes" {
		t.Errorf("aps.attributes-type = %v, want TripActivityAttributes — this string must "+
			"equal the widget bundle's ActivityAttributes struct name EXACTLY, and a "+
			"mismatch fails silently on the device", aps["attributes-type"])
	}

	attrs, ok := aps["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("aps.attributes missing or not an object: %v", aps["attributes"])
	}
	for key, want := range map[string]string{
		"tripId": "trip-1", "vehicleId": "veh-1", "vehicleName": "Optimus",
		// THE LEG IS WHAT MAKES THE CARD ADDRESSABLE. ActivityKit hands the app
		// a per-Activity update token the instant the card is created, and
		// registration is anchored on the leg (§7.21.7) — a device that was
		// asleep when the leg opened cannot derive which leg its card is for.
		// Without this key the server can create a card and then never update
		// or end it.
		"legId": "leg-1",
	} {
		if attrs[key] != want {
			t.Errorf("attributes.%s = %v, want %q", key, attrs[key], want)
		}
	}
	if len(attrs) != 4 {
		t.Errorf("attributes carries %d keys (%v), want exactly 4 — the attributes are "+
			"decoded ONCE and can never be updated, so anything that can change while "+
			"the card is alive does not belong here", len(attrs), attrs)
	}

	state, ok := aps["content-state"].(map[string]any)
	if !ok {
		t.Fatalf("aps.content-state missing: %v", aps)
	}
	for key, want := range map[string]any{
		"v":           float64(1),
		"kind":        "trip",
		"status":      "enroute",
		"tripName":    "DFW to LA",
		"vehicleName": "Optimus",
		"destination": "Grand Canyon Village",
	} {
		if state[key] != want {
			t.Errorf("content-state.%s = %v, want %v", key, state[key], want)
		}
	}
	// ETA is ABSOLUTE unix seconds, not a duration — a duration decays silently
	// on a screen the server cannot repaint.
	if got, want := state["eta"], float64(now.Add(23*time.Minute).Unix()); got != want {
		t.Errorf("content-state.eta = %v, want %v", got, want)
	}
	if _, present := state["progress"]; present {
		t.Errorf("content-state carries `progress` on a trip leg (%v) — a leg has no "+
			"server-known endpoints to measure a fraction against, and a baseline that "+
			"moves on a re-route would draw a track that goes backwards", state["progress"])
	}
	if aps["stale-date"] == nil {
		t.Error("aps.stale-date missing; an update with no stale-date is a promise we cannot keep")
	}
	if _, present := aps["dismissal-date"]; present {
		t.Error("aps.dismissal-date on a start")
	}
}

// TestAttributesOnlyOnStart pins the rule at the one place the keys are written:
// attributes on an `update` are accepted by APNs, ignored by the device, and
// indistinguishable from working.
func TestAttributesOnlyOnStart(t *testing.T) {
	now := time.Now()
	tc := tripLegFixture()
	start := &TripActivityStart{TripID: tc.TripID, VehicleID: tc.VehicleID, VehicleName: tc.VehicleName}

	for _, event := range []ActivityEvent{ActivityEventUpdate, ActivityEventEnd} {
		t.Run(string(event), func(t *testing.T) {
			aps := decodeAPS(t, ActivityNotification{
				Event:        event,
				ContentState: tripContentState(tc, now),
				Timestamp:    now,
				// Deliberately set: a caller that leaves it on must not
				// produce attributes on the wire.
				Start: start,
			})
			if _, present := aps["attributes-type"]; present {
				t.Errorf("aps.attributes-type present on an %q", event)
			}
			if _, present := aps["attributes"]; present {
				t.Errorf("aps.attributes present on an %q", event)
			}
		})
	}
}

// TestRideContentStateIsUnchangedByTripKeys is the compatibility assertion that
// matters most: a build compiled before contracts v0.41.0 must keep decoding a
// ride payload byte-identically, so `kind` and `tripName` have to be ABSENT on
// every ride push rather than present-and-empty.
func TestRideContentStateIsUnchangedByTripKeys(t *testing.T) {
	raw, err := json.Marshal(ActivityContentState{
		Version:     ActivityContentStateVersion,
		Status:      "accepted",
		VehicleName: "Optimus",
		Destination: "Home",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"kind", "tripName"} {
		if _, present := got[key]; present {
			t.Errorf("a ride content-state carries %q (%v) — absent means ride, "+
				"permanently, and that is what lets a pre-v0.41.0 build decode this", key, got)
		}
	}
}

// TestTripActivityExpirations pins the three horizons apart. They are three
// separate decisions about three separate shapes, and a shared constant is how
// one of them silently moves another.
func TestTripActivityExpirations(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	dismiss := now.Add(DismissAfter)

	tests := []struct {
		name string
		n    ActivityNotification
		want time.Duration
	}{
		{
			name: "push-to-start outlasts a tunnel but not a leg",
			n:    ActivityNotification{Event: ActivityEventStart, Timestamp: now},
			want: pushToStartRetention,
		},
		{
			name: "an ordinary update expires at its stale-date",
			n:    ActivityNotification{Event: ActivityEventUpdate, Timestamp: now},
			want: StaleAfter,
		},
		{
			name: "an alerting update gets the day's floor",
			n: ActivityNotification{Event: ActivityEventUpdate, Timestamp: now,
				Alert: &ActivityAlert{Title: "t", Body: "b"}},
			want: alertingUpdateRetention,
		},
		{
			name: "an end must outlive the phone being offline",
			n: ActivityNotification{Event: ActivityEventEnd, Timestamp: now,
				DismissalDate: &dismiss},
			want: endPushRetention,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := activityExpiration(tt.n).Sub(now); got != tt.want {
				t.Errorf("expiration = %s after the send, want %s", got, tt.want)
			}
		})
	}
}
