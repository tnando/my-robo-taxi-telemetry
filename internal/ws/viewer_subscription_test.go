package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// TestHub_ViewerReceivesSharedVehicleUpdates is the WebSocket half of the
// MYR-184 access-set widening.
//
// The subscribed set seeds from Authenticator.GetUserVehicles, which now
// returns owned ∪ accepted-shares. Nothing in internal/ws changed, and that is
// precisely the claim worth a test: the widening is supposed to reach the live
// stream for free, and "for free" is exactly the kind of assumption that turns
// out to be wrong at the handshake boundary.
//
// The frame must also arrive under the VIEWER projection, not the owner's — a
// viewer who receives live updates but with the owner's field set would be a
// mask bypass reached through a completely different door than the REST one.
func TestHub_ViewerReceivesSharedVehicleUpdates(t *testing.T) {
	const (
		ownedVehicle  = "v-owned"
		sharedVehicle = "v-shared"
	)

	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	// The access set the widened GetUserVehicles produces for a rider who
	// owns one car and was granted another.
	authStub := &testAuth{
		userID:     "user-viewer",
		vehicleIDs: []string{ownedVehicle, sharedVehicle},
		roleByVehicle: map[string]auth.Role{
			ownedVehicle:  auth.RoleOwner,
			sharedVehicle: auth.RoleViewer,
		},
	}
	srv := newTestServer(t, hub, authStub)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForClients(t, hub, 1)

	t.Run("a shared vehicle's update reaches the viewer", func(t *testing.T) {
		hub.BroadcastMasked(
			sharedVehicle,
			mask.ResourceVehicleState,
			time.Now().UTC().Format(time.RFC3339),
			map[string]any{
				"speed":        42,
				"latitude":     37.7749,
				"longitude":    -122.4194,
				"licensePlate": "8ABC123",
				// Owner-only (MYR-279) — the viewer projection must strip it.
				"vin": "5YJ3E1EA1PF000001",
			},
		)

		got := readMessage(t, conn)
		if got.Type != msgTypeVehicleUpdate {
			t.Fatalf("expected %q, got %q", msgTypeVehicleUpdate, got.Type)
		}
		var pl vehicleUpdatePayload
		if err := json.Unmarshal(got.Payload, &pl); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}

		if pl.VehicleID != sharedVehicle {
			t.Errorf("vehicleId = %q, want %q", pl.VehicleID, sharedVehicle)
		}
		// MYR-602 REVERSED THIS ASSERTION, on an explicit client decision
		// (Thomas, 2026-09-05): "you should really only see live location
		// during an active trip shared with a user" — or an active ride. A
		// standing share is neither. It is durable and remote, and most of the
		// time the grantee is at home while the owner drives alone.
		//
		// So a plain viewer no longer receives the VALUES. They still receive
		// the KEYS, carrying the schema's own no-value spellings, because all
		// three are in vehicle-state.schema.json's `required` array and
		// dropping them would make the frame undecodable for every installed
		// client rather than merely narrower (internal/mask/sentinels.go).
		//
		// The live map this test was written about now belongs to the two
		// window-scoped roles: `ride_member` (MYR-540) and `trip_participant`
		// (MYR-602), which share one field list that is byte-for-byte the
		// pre-MYR-602 viewer set.
		for field, want := range map[string]any{
			"speed":     float64(0),
			"latitude":  float64(0),
			"longitude": float64(0),
		} {
			got, present := pl.Fields[field]
			if !present {
				t.Errorf("%s was dropped from the viewer frame; the schema declares it required: %v", field, pl.Fields)
				continue
			}
			if got != want {
				t.Errorf("%s = %v, want the no-value sentinel %v for a plain viewer", field, got, want)
			}
		}
		if pl.Fields["licensePlate"] != "8ABC123" {
			t.Errorf("the viewer did not receive licensePlate: %v", pl.Fields)
		}
		// ...but the owner-only field is gone.
		if _, leaked := pl.Fields["vin"]; leaked {
			t.Error("the full VIN reached a viewer over the WebSocket — the viewer projection was not applied")
		}
	})

	t.Run("the owned vehicle still delivers the owner projection", func(t *testing.T) {
		hub.BroadcastMasked(
			ownedVehicle,
			mask.ResourceVehicleState,
			time.Now().UTC().Format(time.RFC3339),
			map[string]any{
				"speed": 7,
				"vin":   "5YJ3E1EA1PF000002",
			},
		)

		got := readMessage(t, conn)
		var pl vehicleUpdatePayload
		if err := json.Unmarshal(got.Payload, &pl); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if pl.Fields["vin"] != "5YJ3E1EA1PF000002" {
			t.Errorf("the owner lost their own VIN: %v", pl.Fields)
		}
	})
}

// TestHub_UngrantedVehicleIsNotDelivered is the negative half: widening the
// access set must not have widened it to everything.
func TestHub_UngrantedVehicleIsNotDelivered(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	authStub := &testAuth{
		userID:     "user-viewer",
		vehicleIDs: []string{"v-granted"},
	}
	srv := newTestServer(t, hub, authStub)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForClients(t, hub, 1)

	// A car nobody shared with this person.
	hub.BroadcastMasked(
		"v-somebody-elses",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		map[string]any{"speed": 99, "latitude": 1.0, "longitude": 2.0},
	)
	// ...followed by one that WAS shared, so the test has a deterministic
	// frame to wait for instead of racing a timeout.
	hub.BroadcastMasked(
		"v-granted",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		map[string]any{"speed": 11},
	)

	got := readMessage(t, conn)
	var pl vehicleUpdatePayload
	if err := json.Unmarshal(got.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if pl.VehicleID != "v-granted" {
		t.Fatalf("first frame was for %q — an ungranted vehicle's telemetry was delivered", pl.VehicleID)
	}
}
