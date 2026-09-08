package ws

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// MYR-601 — THE WIDEN ARM OF THE BACKSTOP. Until this existed the sweep only
// narrowed, so every claim of the form "and the 60-second sweep catches it" was
// true for a lost grant and FALSE for a gained one: the widening direction had
// event-driven producers and nothing behind them. These tests are that bound.

// THE HEADLINE. Nobody published anything — the exact shape of a widening
// written by a producer outside this process (the Next.js app inserts
// `"Vehicle"` rows straight into the shared database) or of a publish the bus
// dropped under backpressure. The session must still pick the car up.
func TestAccessRevalidator_ReHandshakesASessionThatGainedAVehicle(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "owner", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	// A second car is linked, and nothing tells the hub.
	authn.set([]string{"veh-1", "veh-2"}, nil)

	rv := NewAccessRevalidator(hub, authn, time.Minute, newSilentLogger())
	if n := rv.SweepOnce(context.Background()); n != 1 {
		t.Fatalf("sweep ended %d sessions, want 1 — a session whose user gained a car "+
			"must re-handshake, or the ≤60s bound is a claim about nothing", n)
	}
	expectClosed4002(t, conn, "owner who gained veh-2")
}

// ONE RE-HANDSHAKE PER USER, NOT PER SESSION. WidenUserAccess closes EVERY
// session the user holds — it cannot find them by a car they are not yet
// authorized for — so a second tab would otherwise publish a second close for
// sessions the first one already ended.
func TestAccessRevalidator_WidensAUserOncePerPass(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "owner", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	first := dialAndAuth(t, srv.URL, "tok")
	defer first.Close(websocket.StatusNormalClosure, "test done")
	second := dialAndAuth(t, srv.URL, "tok")
	defer second.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 2)

	authn.set([]string{"veh-1", "veh-2"}, nil)

	// Two sessions, ONE widening: the first call closes both, so the pass
	// reports the two sessions it ended and never publishes a second time.
	if n := NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background()); n != 2 {
		t.Fatalf("sweep ended %d sessions, want both of this user's 2", n)
	}
	expectClosed4002(t, first, "first tab")
	expectClosed4002(t, second, "second tab")
}

// A LOSS STILL WINS OVER A GAIN. A session that must be closed for a revoked
// car is closed as a REVOCATION, and the reconnect resolves the gain too — one
// close, and the narrowing keeps its own reason and its own counter, which is
// what stops a security signal being diluted by a convenience.
func TestAccessRevalidator_LossWinsWhenASessionBothLostAndGained(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	// veh-1 revoked, veh-2 granted, in the same interval.
	authn.set([]string{"veh-2"}, nil)

	if n := NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background()); n != 1 {
		t.Fatalf("sweep ended %d sessions, want exactly 1 — the session must be closed "+
			"ONCE, by the loss", n)
	}
	expectClosed4002(t, conn, "viewer who lost one car and gained another")
}

// AN UNCHANGED SESSION IS STILL LEFT ALONE, and a wildcard client is still not
// read as "has gained everything". The dev-mode sentinel yields no per-vehicle
// entries in either direction, and reading it wrongly here would re-handshake
// every dev client once a minute — the mirror of the narrowing arm's own
// wildcard trap.
func TestAccessRevalidator_WidenArmSparesUnchangedAndWildcardSessions(t *testing.T) {
	t.Run("nothing changed", func(t *testing.T) {
		hub := NewHub(newSilentLogger(), NoopHubMetrics{})
		defer hub.Stop()

		authn := &mutableAuth{userID: "owner", vehicleIDs: []string{"veh-1"}}
		srv := newTestServer(t, hub, authn)
		defer srv.Close()
		conn := dialAndAuth(t, srv.URL, "tok")
		defer conn.Close(websocket.StatusNormalClosure, "test done")
		waitForClients(t, hub, 1)

		if n := NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background()); n != 0 {
			t.Fatalf("sweep ended %d sessions with nothing changed, want 0", n)
		}
		expectStillOpen(t, conn, "unchanged owner")
	})

	t.Run("a dev-mode wildcard client", func(t *testing.T) {
		hub := NewHub(newSilentLogger(), NoopHubMetrics{})
		defer hub.Stop()

		authn := &NoopAuthenticator{UserID: "dev"}
		srv := newTestServer(t, hub, authn)
		defer srv.Close()
		conn := dialAndAuth(t, srv.URL, "tok")
		defer conn.Close(websocket.StatusNormalClosure, "test done")
		waitForClients(t, hub, 1)

		if n := NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background()); n != 0 {
			t.Fatalf("sweep ended %d dev-mode sessions, want 0", n)
		}
		expectStillOpen(t, conn, "dev-mode wildcard client")
	})
}
