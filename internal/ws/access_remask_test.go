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

// TestAccessRevalidator_RemaskResendsTheSnapshot is finding 10: a re-mask that
// changes only future frames is HALF a change, and it is half in both
// directions.
//
// A WebSocket client holds a MERGED state assembled from every frame it has
// received. Narrowed, its last REAL coordinate is never overwritten, because
// from then on a location-only delta projects to nothing but sentinels and is
// suppressed by IsSubstantiveExcludingSentinels — correctly, since its arrival
// alone would be a "this car is streaming" beacon. Promoted, the SENTINEL zeros
// its connect-time snapshot delivered are never corrected until the car happens
// to move, which a parked car never does.
//
// Both are answered by re-delivering the snapshot through the NEW mask.
func TestAccessRevalidator_RemaskResendsTheSnapshot(t *testing.T) {
	const vehicleID = "veh-1"
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		startAt time.Time
		moveTo  time.Time
		// wantLat is the latitude the re-delivered snapshot must carry.
		wantLat float64
		why     string
	}{
		{
			name:    "narrowed: the stale real coordinate is overwritten with the sentinel",
			startAt: base.Add(time.Hour),
			moveTo:  base.Add(25 * time.Hour),
			wantLat: 0,
			why: "the client would otherwise show the car frozen at its last real " +
				"position for the whole life of the connection",
		},
		{
			name:    "promoted: the sentinel zeros are overwritten with the real coordinate",
			startAt: base.Add(-time.Hour),
			moveTo:  base.Add(time.Hour),
			wantLat: 37.7749,
			why: "a parked car sends no location group, so the participant's map would " +
				"show Null Island for as long as it stayed still",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeSnapshotReader{snapshots: gpsOnlySnapshot()}
			hub := NewHub(newSilentLogger(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
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

			// The connect-time snapshot's own GPS frame, read rather than
			// drained: a read that times out on this transport CLOSES the
			// socket, so the test consumes exactly what it expects.
			first, ok := readUntilField(t, conn, "latitude")
			if !ok {
				t.Fatal("the handshake delivered no snapshot carrying `latitude`")
			}
			if first == tt.wantLat {
				t.Fatalf("the handshake already delivered latitude %v, so this case "+
					"cannot show that the RE-MASK delivered anything", first)
			}

			authn.advanceTo(tt.moveTo)
			NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background())

			lat, ok := readUntilField(t, conn, "latitude")
			if !ok {
				t.Fatalf("the re-mask delivered no snapshot carrying `latitude`; %s", tt.why)
			}
			if lat != tt.wantLat {
				t.Errorf("latitude after the re-mask = %v, want %v; %s", lat, tt.wantLat, tt.why)
			}
		})
	}
}

// readUntilField reads frames until one carries the named field, or the socket
// goes quiet.
func readUntilField(t *testing.T, conn *websocket.Conn, field string) (float64, bool) {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, raw, err := conn.Read(ctx)
		cancel()
		if err != nil {
			return 0, false
		}
		var msg wsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if msg.Type != msgTypeVehicleUpdate {
			continue
		}
		var pl vehicleUpdatePayload
		if err := json.Unmarshal(msg.Payload, &pl); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		v, present := pl.Fields[field]
		if !present {
			continue
		}
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("%s = %v (%T), want a number", field, v, v)
		}
		return f, true
	}
}

// TestBroadcastMasked_ViewerGetsNothingFromALocationOnlyFrame is the shape the
// re-mask re-delivery exists BECAUSE of, asserted directly.
//
// The production broadcast path injects `lastUpdated` into every non-nav frame
// before masking (nav_broadcast.go), so a GPS-group flush reaches the mask as
// {latitude, longitude, heading, lastUpdated} — and for a plain viewer that
// projects to three sentinels plus an envelope key, which is not empty. Only
// IsSubstantiveExcludingSentinels suppresses it, and it must: the values say
// nothing, but the frame's ARRIVAL, once a second, says the car is streaming
// right now — the MYR-435 timing leak rebuilt out of zeros.
//
// The consequence, and the reason the sweep re-delivers a snapshot: a client
// narrowed from trip_participant to viewer will never again be sent a frame
// that could correct the real coordinate it is already holding.
func TestBroadcastMasked_ViewerGetsNothingFromALocationOnlyFrame(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	viewer := testClient(hub, "cviewer")
	viewer.vehicleIDs = []string{"veh-1"}
	viewer.subscribed = map[string]struct{}{"veh-1": {}}
	viewer.setRoles(map[string]auth.Role{"veh-1": auth.RoleViewer})

	participant := testClient(hub, "cparticipant")
	participant.vehicleIDs = []string{"veh-1"}
	participant.subscribed = map[string]struct{}{"veh-1": {}}
	participant.setRoles(map[string]auth.Role{"veh-1": auth.RoleTripParticipant})

	hub.BroadcastMasked("veh-1", mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		// EXACTLY the production shape: the GPS atomic group, plus the
		// freshness key the broadcast path adds to every non-nav frame.
		map[string]any{
			"latitude":    37.7749,
			"longitude":   -122.4194,
			"heading":     float64(90),
			"lastUpdated": "2026-09-06T12:00:00Z",
		},
	)

	assertNoFrame(t, viewer)

	got := drainOne(t, participant)
	if got.Type != msgTypeVehicleUpdate {
		t.Fatalf("participant frame type = %q, want %q", got.Type, msgTypeVehicleUpdate)
	}
	var pl vehicleUpdatePayload
	if err := json.Unmarshal(got.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if pl.Fields["latitude"] != 37.7749 {
		t.Errorf("a trip participant inside the window must receive the real coordinate: %v", pl.Fields)
	}
}
