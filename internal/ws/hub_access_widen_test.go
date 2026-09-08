package ws

import (
	"testing"
	"time"

	"github.com/coder/websocket"
)

// MYR-609 — the WIDENING half of websocket-protocol.md §10 DV-09. Same real
// hub and real sockets as hub_access_test.go, for the same reason: what is
// being fixed is that every struct involved looked correct while the grantee's
// map stayed one car short for the life of the session.

// THE VEHICLE CANNOT BE USED TO FIND THE SESSIONS, and this is the whole
// reason WidenUserAccess exists rather than a call to RevokeUserAccess with the
// new car's id. A grantee who just GAINED a car is by definition not yet
// authorized for it, so the narrowing path's per-vehicle filter matches nothing
// — the widening would compile, run, log, and do nothing at all.
func TestWidenUserAccess_ClosesSessionsTheVehicleFilterCannotFind(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	// The session was handshaked BEFORE the extend, so it carries only veh-1.
	srv := newTestServer(t, hub, &testAuth{userID: "grantee", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	// The narrowing path, given the newly granted car, finds nobody. This is
	// the negative control: without it the test below could pass for the wrong
	// reason.
	if n := hub.RevokeUserAccess("grantee", "veh-2", "extended"); n != 0 {
		t.Fatalf("RevokeUserAccess matched %d sessions for a car the client is not yet "+
			"authorized for, want 0 — if this ever matches, the widening does not need its "+
			"own path", n)
	}
	// Asserted on the hub rather than by reading the socket: a read that times
	// out closes the connection underneath us, and this one has to survive to
	// receive the widening's close frame below.
	if got := hub.ClientCount(); got != 1 {
		t.Fatalf("clients = %d after the no-op revoke, want 1", got)
	}

	if n := hub.WidenUserAccess("grantee", "veh-2", "extended"); n != 1 {
		t.Fatalf("WidenUserAccess closed %d sessions, want 1 — every session this user holds "+
			"is re-handshaked, because none of them can be found by the new car", n)
	}

	// The SAME 4002 frame as a narrowing, deliberately: §6.2 already defines it
	// as "reconnect, then render whatever the new handshake returns", which is
	// exactly the behavior a widening wants, and every deployed SDK has it.
	expectClosed4002(t, conn, "grantee")
	waitForClients(t, hub, 0)
}

// Only the named user is re-handshaked. The owner who pressed the button gained
// nothing and must not be disconnected, and neither must anybody else.
func TestWidenUserAccess_LeavesEveryOtherUserAlone(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	granteeSrv := newTestServer(t, hub, &testAuth{userID: "grantee", vehicleIDs: []string{"veh-1"}})
	defer granteeSrv.Close()
	ownerSrv := newTestServer(t, hub, &testAuth{userID: "owner", vehicleIDs: []string{"veh-1", "veh-2"}})
	defer ownerSrv.Close()

	grantee := dialAndAuth(t, granteeSrv.URL, "tok")
	defer grantee.Close(websocket.StatusNormalClosure, "test done")
	owner := dialAndAuth(t, ownerSrv.URL, "tok")
	defer owner.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 2)

	hub.WidenUserAccess("grantee", "veh-2", "extended")

	expectClosed4002(t, grantee, "grantee")
	expectStillOpen(t, owner, "owner")
	waitForClientsWithin(t, hub, 1, 5*time.Second)
}

// An empty user id is a NO-OP, not a wildcard. The guard lives here as well as
// in the handler because the cost of getting it wrong is every connected client
// on the process being disconnected by one extend.
func TestWidenUserAccess_EmptyUserIsANoOp(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "grantee", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	if n := hub.WidenUserAccess("", "veh-2", "extended"); n != 0 {
		t.Errorf("WidenUserAccess(\"\") closed %d sessions, want 0 — an empty id must never "+
			"mean everybody", n)
	}
	expectStillOpen(t, conn, "grantee")
}

// Widening a user with no live sessions is the COMMON case — most grantees are
// not connected when an owner extends — and must be a silent, cheap no-op that
// the owner's 201 never waits on.
func TestWidenUserAccess_NobodyConnectedIsSilent(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	if n := hub.WidenUserAccess("grantee", "veh-2", "extended"); n != 0 {
		t.Errorf("closed %d sessions with nobody connected, want 0", n)
	}
	// And it is idempotent: the second call is the same no-op, which is what a
	// retried publish on the bus looks like.
	if n := hub.WidenUserAccess("grantee", "veh-2", "extended"); n != 0 {
		t.Errorf("second call closed %d sessions, want 0", n)
	}
}
