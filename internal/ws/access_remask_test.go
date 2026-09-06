package ws

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// The MYR-602 WINDOW-EDGE RE-MASK. A trip window opens and closes on the CLOCK,
// so unlike a revoked share there is no mutation anywhere to hang the fast
// nudge off — this sweep IS the mechanism, and these tests drive it with a fake
// clock rather than waiting on a real one.

// clockedAuth is an Authenticator whose per-vehicle role is a function of a
// FAKE CLOCK and a window, which is exactly the shape the real trip access
// query has: `starts_at <= NOW() AND NOW() < COALESCE(ended_at, ends_at)`.
//
// Modelling the window rather than hard-setting the role is what makes these
// tests about the thing that actually breaks: nobody CALLS anything at the
// edge, the answer simply changes because time passed.
type clockedAuth struct {
	mu sync.Mutex

	userID     string
	vehicleIDs []string
	now        time.Time
	// windowStart/windowEnd bound the trip. Inside it the caller resolves
	// trip_participant; outside it, the plain viewer their standing share
	// gives them.
	windowStart time.Time
	windowEnd   time.Time
}

func (a *clockedAuth) ValidateToken(_ context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	return a.userID, nil
}

func (a *clockedAuth) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.vehicleIDs...), nil
}

func (a *clockedAuth) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.now.Before(a.windowStart) && a.now.Before(a.windowEnd) {
		return auth.RoleTripParticipant, nil
	}
	return auth.RoleViewer, nil
}

func (a *clockedAuth) advanceTo(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = t
}

// connectedClient returns the hub's single registered client.
func connectedClient(t *testing.T, hub *Hub) *Client {
	t.Helper()
	clients := hub.snapshotClients()
	if len(clients) != 1 {
		t.Fatalf("hub holds %d clients, want 1", len(clients))
	}
	return clients[0]
}

func TestAccessRevalidator_RemasksAcrossTripWindowEdges(t *testing.T) {
	const vehicleID = "veh-trip"
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// at is where the fake clock starts (deciding the handshake role) and
		// then moves to (deciding the swept role).
		startAt  time.Time
		moveTo   time.Time
		wantFrom auth.Role
		wantTo   auth.Role
	}{
		{
			name:     "window opens: a share-holder is promoted without reconnecting",
			startAt:  base.Add(-time.Hour),
			moveTo:   base.Add(time.Hour),
			wantFrom: auth.RoleViewer,
			wantTo:   auth.RoleTripParticipant,
		},
		{
			name:     "window closes: a participant drops to viewer, session intact",
			startAt:  base.Add(time.Hour),
			moveTo:   base.Add(25 * time.Hour),
			wantFrom: auth.RoleTripParticipant,
			wantTo:   auth.RoleViewer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewHub(newSilentLogger(), NoopHubMetrics{})
			t.Cleanup(hub.Stop)

			authn := &clockedAuth{
				userID:      "participant",
				vehicleIDs:  []string{vehicleID},
				now:         tt.startAt,
				windowStart: base,
				windowEnd:   base.Add(24 * time.Hour),
			}
			srv := newTestServer(t, hub, authn)
			t.Cleanup(srv.Close)

			conn := dialAndAuth(t, srv.URL, "tok")
			defer conn.Close(websocket.StatusNormalClosure, "test done")
			waitForClients(t, hub, 1)

			client := connectedClient(t, hub)
			if got := client.roleFor(vehicleID); got != tt.wantFrom {
				t.Fatalf("handshake role = %q, want %q", got, tt.wantFrom)
			}

			// The edge: nothing is called, nothing is published. Time passes.
			authn.advanceTo(tt.moveTo)

			rv := NewAccessRevalidator(hub, authn, 0, newSilentLogger())
			if closed := rv.SweepOnce(context.Background()); closed != 0 {
				t.Fatalf("sweep closed %d sessions; a window edge must RE-MASK, not kick — "+
					"the share is intact and the vehicle never left the access set", closed)
			}
			if got := client.roleFor(vehicleID); got != tt.wantTo {
				t.Errorf("role after the sweep = %q, want %q", got, tt.wantTo)
			}
		})
	}
}

// TestAccessRevalidator_RemaskChangesWhatTheSocketDelivers is the assertion
// that matters operationally: the role table is only interesting because the
// broadcast reads it. A participant whose window closed must stop receiving the
// car's position on the SAME connection.
func TestAccessRevalidator_RemaskChangesWhatTheSocketDelivers(t *testing.T) {
	const vehicleID = "veh-trip"
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	authn := &clockedAuth{
		userID:      "participant",
		vehicleIDs:  []string{vehicleID},
		now:         base.Add(time.Hour),
		windowStart: base,
		windowEnd:   base.Add(24 * time.Hour),
	}
	srv := newTestServer(t, hub, authn)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	payload := map[string]any{
		"speed":       float64(42),
		"latitude":    37.7749,
		"longitude":   -122.4194,
		"chargeLevel": float64(80),
	}
	stamp := time.Now().UTC().Format(time.RFC3339)

	hub.BroadcastMasked(vehicleID, mask.ResourceVehicleState, stamp, payload)
	inside := readVehicleUpdateFields(t, conn)
	if inside["latitude"] != 37.7749 {
		t.Fatalf("inside the window a participant must receive real GPS, got %v", inside)
	}

	// The window closes with nobody doing anything.
	authn.advanceTo(base.Add(25 * time.Hour))
	NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background())

	hub.BroadcastMasked(vehicleID, mask.ResourceVehicleState, stamp, payload)
	outside := readVehicleUpdateFields(t, conn)
	if outside["latitude"] != float64(0) || outside["longitude"] != float64(0) {
		t.Errorf("after the window closed the socket still carried real GPS: %v", outside)
	}
	if outside["speed"] != float64(0) {
		t.Errorf("after the window closed the socket still carried real speed: %v", outside)
	}
	if outside["chargeLevel"] != float64(80) {
		t.Errorf("the catalog half of the row was lost too (%v) — a narrowing must take "+
			"location, not the whole frame", outside)
	}
}

// TestAccessRevalidator_KeepsRoleWhenResolutionFails pins the fail-open
// direction. A database blip must delay a narrowing by one interval, never
// black out a live map and never hand out a stronger role.
func TestAccessRevalidator_KeepsRoleWhenResolutionFails(t *testing.T) {
	const vehicleID = "veh-trip"
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	authn := &failingRoleAuth{userID: "participant", vehicleIDs: []string{vehicleID}}
	srv := newTestServer(t, hub, authn)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	client := connectedClient(t, hub)
	if got := client.roleFor(vehicleID); got != auth.RoleTripParticipant {
		t.Fatalf("handshake role = %q, want %q", got, auth.RoleTripParticipant)
	}

	authn.startFailing()
	NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background())

	if got := client.roleFor(vehicleID); got != auth.RoleTripParticipant {
		t.Errorf("role after a failed resolution = %q, want it UNCHANGED at %q — a blip "+
			"must not invent a role in either direction", got, auth.RoleTripParticipant)
	}
}

// failingRoleAuth resolves once (at handshake) and then errors forever.
type failingRoleAuth struct {
	mu         sync.Mutex
	userID     string
	vehicleIDs []string
	failing    bool
}

func (a *failingRoleAuth) ValidateToken(_ context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	return a.userID, nil
}

func (a *failingRoleAuth) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	return append([]string(nil), a.vehicleIDs...), nil
}

func (a *failingRoleAuth) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failing {
		return auth.Role(""), context.DeadlineExceeded
	}
	return auth.RoleTripParticipant, nil
}

func (a *failingRoleAuth) startFailing() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failing = true
}

// readVehicleUpdateFields reads one frame and returns its `fields` map.
func readVehicleUpdateFields(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	msg := readMessage(t, conn)
	if msg.Type != msgTypeVehicleUpdate {
		t.Fatalf("frame type = %q, want %q", msg.Type, msgTypeVehicleUpdate)
	}
	var pl vehicleUpdatePayload
	if err := json.Unmarshal(msg.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return pl.Fields
}
