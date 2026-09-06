package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// The `drive_started` coordinate leak (MYR-602).
//
// This frame rode the role-BLIND Hub.Broadcast, whose own doc comment justified
// that by listing drive_started as carrying "no role-restricted fields". It
// carries `startLocation` — the car's raw position at the moment it pulled away,
// which is the single most locating thing a car emits: a home and a workplace,
// twice a day, indefinitely.

func readDriveStarted(t *testing.T, conn *websocket.Conn) driveStartedPayload {
	t.Helper()
	msg := readMessage(t, conn)
	if msg.Type != msgTypeDriveStarted {
		t.Fatalf("frame type = %q, want %q", msg.Type, msgTypeDriveStarted)
	}
	var pl driveStartedPayload
	if err := json.Unmarshal(msg.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return pl
}

func TestBroadcastByLocationAccess_DriveStartedCoordinate(t *testing.T) {
	const vehicleID = "veh-1"
	const lat, lng = 37.7749, -122.4194

	tests := []struct {
		name    string
		role    auth.Role
		wantLat float64
		wantLng float64
	}{
		{"owner reads the real coordinate", auth.RoleOwner, lat, lng},
		{"ride member reads the real coordinate", auth.RoleRideMember, lat, lng},
		{"trip participant reads the real coordinate", auth.RoleTripParticipant, lat, lng},
		{"plain viewer reads the no-fix sentinel", auth.RoleViewer, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewHub(newSilentLogger(), NoopHubMetrics{})
			t.Cleanup(hub.Stop)

			a := &testAuth{
				userID:        "user-1",
				vehicleIDs:    []string{vehicleID},
				roleByVehicle: map[string]auth.Role{vehicleID: tt.role},
			}
			srv := newTestServer(t, hub, a)
			t.Cleanup(srv.Close)

			conn := dialAndAuth(t, srv.URL, "tok")
			defer conn.Close(websocket.StatusNormalClosure, "test done")
			waitForClients(t, hub, 1)

			frame := driveStartedPayload{
				VehicleID:     vehicleID,
				DriveID:       "drive-1",
				StartLocation: startLocation{Latitude: lat, Longitude: lng},
				Timestamp:     time.Now().UTC().Format(time.RFC3339),
			}
			located, err := marshalWSMessage(msgTypeDriveStarted, frame)
			if err != nil {
				t.Fatalf("marshal located: %v", err)
			}
			redactedFrame := frame
			redactedFrame.StartLocation = redactedStartLocation()
			redacted, err := marshalWSMessage(msgTypeDriveStarted, redactedFrame)
			if err != nil {
				t.Fatalf("marshal redacted: %v", err)
			}

			hub.BroadcastByLocationAccess(vehicleID, located, redacted)

			got := readDriveStarted(t, conn)
			// THE EVENT REACHES EVERY ROLE. Withholding it from a viewer would
			// make their drive list disagree with the owner's about whether a
			// drive happened, and the fact of a drive is already visible to
			// them through the car's `status`.
			if got.DriveID != "drive-1" {
				t.Errorf("driveId = %q, want drive-1 — the EVENT must reach every role", got.DriveID)
			}
			if got.StartLocation.Latitude != tt.wantLat || got.StartLocation.Longitude != tt.wantLng {
				t.Errorf("startLocation = (%v, %v), want (%v, %v)",
					got.StartLocation.Latitude, got.StartLocation.Longitude, tt.wantLat, tt.wantLng)
			}
		})
	}
}

// TestBroadcastByLocationAccess_NilRedactedWithholds covers the other half of
// the contract: a caller that has no safe shape to send may pass nil, and those
// clients get nothing rather than a coordinate.
func TestBroadcastByLocationAccess_NilRedactedWithholds(t *testing.T) {
	const vehicleID = "veh-1"
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{vehicleID},
		roleByVehicle: map[string]auth.Role{vehicleID: auth.RoleViewer},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	located, err := marshalWSMessage(msgTypeDriveStarted, driveStartedPayload{
		VehicleID:     vehicleID,
		DriveID:       "withheld",
		StartLocation: startLocation{Latitude: 1, Longitude: 2},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	hub.BroadcastByLocationAccess(vehicleID, located, nil)

	// Then a frame that IS for this viewer, so the assertion is about ordering
	// rather than about a read that times out either way.
	follow, err := marshalWSMessage(msgTypeDriveEnded, driveEndedPayload{
		VehicleID: vehicleID, DriveID: "delivered",
	})
	if err != nil {
		t.Fatalf("marshal follow: %v", err)
	}
	hub.Broadcast(vehicleID, follow)

	msg := readMessage(t, conn)
	if msg.Type != msgTypeDriveEnded {
		t.Fatalf("first frame was %q, want %q — the withheld drive_started reached the viewer",
			msg.Type, msgTypeDriveEnded)
	}
}
