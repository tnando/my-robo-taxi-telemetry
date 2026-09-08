package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// MYR-609 end to end through the REAL bus, the mirror of
// share_access_dispatcher_test.go: §7.5.8 extend calls the notifier on one
// side, a real WebSocket client is re-handshaked on the other, nothing stubbed
// in between.

// TestShareWiden_ExtendReHandshakesTheGrantee is the scenario the endpoint
// exists inside: the grantee is connected, holding a session handshaked before
// the extend, so their `vehicleIDs` cannot contain the new car no matter what
// the database says. Something has to make a next handshake happen.
func TestShareWiden_ExtendReHandshakesTheGrantee(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()

	if _, err := newShareWidenDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Authorized for the car they already had — never for the one being added.
	srv := newWSTestServer(t, hub, &fakeAuth{userID: "grantee", vehicleIDs: []string{"veh-had"}})
	defer srv.Close()
	conn := dialWSAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitClients(t, hub, 1)

	// Exactly what ShareInviteHandler.ServeExtend does once the grant commits
	// and the grantee's cached access set has been busted.
	start := time.Now()
	newShareWidenBusNotifier(bus, logger).ShareAccessWidened("grantee", "veh-gained", "extended")

	latency := awaitClose4002(t, conn, start, "extended grantee")
	if latency > 2*time.Second {
		t.Errorf("the widening took %s to reach the socket; the owner is looking at a share "+
			"their grantee's map does not have", latency)
	}
	t.Logf("extend → live socket re-handshaked in %s", latency)
}

// The OWNER who pressed the button gained nothing and must keep streaming. The
// widening is addressed at one person, exactly like the revocation beside it.
func TestShareWiden_OwnerKeepsStreaming(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()
	if _, err := newShareWidenDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ownerSrv := newWSTestServer(t, hub, &fakeAuth{userID: "owner", vehicleIDs: []string{"veh-gained"}})
	defer ownerSrv.Close()
	granteeSrv := newWSTestServer(t, hub, &fakeAuth{userID: "grantee", vehicleIDs: []string{"veh-had"}})
	defer granteeSrv.Close()

	ownerConn := dialWSAuth(t, ownerSrv.URL, "tok")
	defer ownerConn.Close(websocket.StatusNormalClosure, "test done")
	granteeConn := dialWSAuth(t, granteeSrv.URL, "tok")
	defer granteeConn.Close(websocket.StatusNormalClosure, "test done")
	waitClients(t, hub, 2)

	newShareWidenBusNotifier(bus, logger).ShareAccessWidened("grantee", "veh-gained", "extended")
	awaitClose4002(t, granteeConn, time.Now(), "grantee")

	hub.Broadcast("veh-gained", []byte(`{"type":"vehicle_update"}`))
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := ownerConn.Read(readCtx); err != nil {
		t.Fatalf("the OWNER lost their stream when they extended a share: %v", err)
	}
}

// A WIDENING IS NOT A REVOCATION, and the separate topic is what keeps anything
// that watches, counts or alerts on revocations from having conveniences folded
// into its numbers. A subscriber to the revocation topic must never see this.
func TestShareWiden_DoesNotPublishOntoTheRevocationTopic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	revocations := make(chan events.Event, 1)
	if _, err := bus.Subscribe(events.TopicShareAccessRevoked, func(e events.Event) { revocations <- e }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	widenings := make(chan events.Event, 1)
	if _, err := bus.Subscribe(events.TopicShareAccessWidened, func(e events.Event) { widenings <- e }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	newShareWidenBusNotifier(bus, logger).ShareAccessWidened("grantee", "veh-gained", "extended")

	select {
	case <-widenings:
	case <-time.After(2 * time.Second):
		t.Fatal("the widening was never published")
	}
	select {
	case e := <-revocations:
		t.Fatalf("a widening arrived on the REVOCATION topic: %+v", e.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// An empty grantee publishes NOTHING. Reaching the hub as a wildcard would
// disconnect every connected client on the process because one extend could not
// name its beneficiary.
func TestShareWidenNotifier_IgnoresEmptyGrantee(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	received := make(chan events.Event, 1)
	if _, err := bus.Subscribe(events.TopicShareAccessWidened, func(e events.Event) { received <- e }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	newShareWidenBusNotifier(bus, logger).ShareAccessWidened("", "veh-gained", "extended")

	select {
	case e := <-received:
		t.Fatalf("published an event for an empty grantee: %+v", e.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// The dispatcher runs on the bus's per-subscription goroutine, so a panic here
// would not fail one widening — it would silently kill EVERY later one on the
// topic while the server carried on looking healthy. Each case is an ordinary
// no-op.
func TestShareWidenDispatcher_SurvivesMalformedInput(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	tests := []struct {
		name    string
		withHub bool
		evt     events.Event
	}{
		{
			name:    "a payload of the wrong type",
			withHub: true,
			evt: events.NewEvent(events.ShareAccessRevokedEvent{
				GranteeUserID: "grantee", VehicleID: "veh-1", Reason: "revoked",
			}),
		},
		{
			name:    "an empty grantee, which is not a wildcard",
			withHub: true,
			evt:     events.NewEvent(events.ShareAccessWidenedEvent{VehicleID: "veh-1", Reason: "extended"}),
		},
		{
			name: "no hub wired at all",
			evt: events.NewEvent(events.ShareAccessWidenedEvent{
				GranteeUserID: "grantee", VehicleID: "veh-1", Reason: "extended",
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hub *ws.Hub
			if tt.withHub {
				hub = ws.NewHub(logger, &countingHubMetrics{})
				defer hub.Stop()
			}
			newShareWidenDispatcher(hub, logger).handle(tt.evt)
			if tt.withHub && hub.ClientCount() != 0 {
				t.Errorf("hub client count = %d, want 0", hub.ClientCount())
			}
		})
	}
}
