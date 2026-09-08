package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// MYR-601 review round — ONE REAL-BUS ASSERTION PER WIRED WIDENING PRODUCER.
//
// The provisioning hook already had one (owner_stream_hook_access_test.go);
// redeem, the un-suspending PATCH and the ride join were asserted only at the
// handler seam, against a recording fake. That proves the handler calls
// something. It does not prove that what production hands it reaches a socket,
// which is the entire failure MYR-601 was reported from: every individual
// piece was correct and nothing connected them.
//
// Each test below drives the real HTTP route on a mux, through the real
// handler constructed with the REAL wiring option (shareRedeemWidenOption and
// friends — the same line setupVehicleSharingEndpoints runs), onto the real
// events bus, through the real shareWidenDispatcher, into a real ws.Hub, and
// asserts a real WebSocket client is closed 4002.
//
// WHAT IS FAKED AND WHY: the stores, and only the stores. Every one of these
// endpoints reaches a `*store.VehicleShareRepo` / `*store.RideRequestRepo`
// before it can widen anything, and those need a live Postgres — which is also
// why the mux cannot be the one `setupVehicleSharingEndpoints` builds. The
// fakes embed the package's own store interfaces and override exactly the one
// method the path calls, so a widened interface cannot silently make a fake
// answer a question it was never given.

// --- the fakes -------------------------------------------------------------

// fakeWidenTokens authenticates every request as one user. The handlers take an
// unexported `tokenValidator`; this satisfies it structurally.
type fakeWidenTokens struct{ userID string }

func (f *fakeWidenTokens) ValidateToken(context.Context, string) (string, error) {
	return f.userID, nil
}

// fakeWidenRedeemStore answers the one redemption the test performs. The
// embedded interface is nil, so any OTHER method the handler learns to call
// panics loudly instead of returning a plausible zero value.
type fakeWidenRedeemStore struct {
	telemetry.ShareRedeemStore
	vehicleID string
}

func (f *fakeWidenRedeemStore) RedeemCode(context.Context, string, string) ([]telemetry.ShareGrantRow, error) {
	return []telemetry.ShareGrantRow{{VehicleID: f.vehicleID, OwnerUserID: "cowner"}}, nil
}

func (f *fakeWidenRedeemStore) OwnerFirstName(context.Context, string) (string, error) {
	return "Alex", nil
}

// fakeWidenSharedLister is the catalog read the redeem response builds from.
type fakeWidenSharedLister struct{ telemetry.SharedVehicleLister }

func (f *fakeWidenSharedLister) ListSharedByIDs(
	context.Context, string, []string,
) ([]telemetry.SharedVehicleRow, error) {
	return nil, nil
}

// fakeWidenInviteStore answers the one PATCH the test performs.
type fakeWidenInviteStore struct {
	telemetry.ShareInviteStore
	granteeID string
	vehicleID string
}

func (f *fakeWidenInviteStore) PatchInvite(
	context.Context, string, string, telemetry.ShareInvitePatch,
) (telemetry.ShareInviteRow, string, error) {
	return telemetry.ShareInviteRow{
		ID:        "inv_widen_1",
		VehicleID: f.vehicleID,
		Label:     "A friend",
		Status:    "accepted",
		// Suspended FALSE is the resulting row of an un-suspend — and is also
		// what an allowRides-only patch returns, which is why the handler reads
		// the REQUEST for this branch.
		Grant:     auth.ShareGrant{},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, f.granteeID, nil
}

// fakeWidenRideStore answers the one join the test performs.
type fakeWidenRideStore struct {
	telemetry.RideRequestStore
	vehicleID string
}

func (f *fakeWidenRideStore) JoinByCode(
	_ context.Context, _, userID string,
) (telemetry.RideRequestData, bool, error) {
	return telemetry.RideRequestData{
		ID:        "ride_widen_1",
		RiderID:   "crider",
		OwnerID:   "cowner",
		VehicleID: f.vehicleID,
		Status:    "accepted",
		Members:   []telemetry.RideMemberData{{UserID: userID, FirstName: "Sam"}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, true, nil
}

// --- the harness -----------------------------------------------------------

// widenProducerRig is everything the three tests share: a real bus with the
// real dispatcher on it, a real hub, and one connected client whose access set
// does NOT contain the car it is about to gain.
type widenProducerRig struct {
	deps httpRouteDeps
	conn *websocket.Conn
	mux  *http.ServeMux
}

func newWidenProducerRig(t *testing.T, userID string) *widenProducerRig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	t.Cleanup(hub.Stop)
	if _, err := newShareWidenDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Authorized for a car they already had — never for the one they gain.
	srv := newWSTestServer(t, hub, &fakeAuth{userID: userID, vehicleIDs: []string{"veh-had"}})
	t.Cleanup(srv.Close)
	conn := dialWSAuth(t, srv.URL, "tok")
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "test done") })
	waitClients(t, hub, 1)

	return &widenProducerRig{
		deps: httpRouteDeps{shareAccessWidener: newShareWidenBusNotifier(bus, logger)},
		conn: conn,
		mux:  http.NewServeMux(),
	}
}

// call drives one request through the mux and fails on anything but a 200 —
// a producer that refused the mutation would announce nothing, and the socket
// assertion afterwards would then be asserting the wrong thing.
func (r *widenProducerRig) call(t *testing.T, method, path, body string) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s = %d, want 200 (body %s)", method, path, rec.Code, rec.Body.String())
	}
}

// --- the three producers ---------------------------------------------------

// §7.5.5 REDEEM. The "you're in!" screen is followed by a tracking sheet, on a
// session whose access set was frozen before the grant existed.
func TestWidenProducerE2E_RedeemReHandshakesTheRedeemer(t *testing.T) {
	rig := newWidenProducerRig(t, "credeemer")
	h := telemetry.NewShareRedeemHandler(
		&fakeWidenTokens{userID: "credeemer"},
		&fakeWidenRedeemStore{vehicleID: "veh-gained"},
		&fakeWidenSharedLister{},
		nil, // no cache in this process to bust; the socket is what is under test
		testLogger(),
		shareRedeemWidenOption(rig.deps),
	)
	rig.mux.HandleFunc("POST /api/invites/redeem", h.ServeHTTP)

	start := time.Now()
	rig.call(t, http.MethodPost, "/api/invites/redeem", `{"code":"RBO246"}`)

	latency := awaitClose4002(t, rig.conn, start, "the redeemer")
	t.Logf("redeem → live socket re-handshaked in %s", latency)
}

// §7.5.4 PATCH, the UN-SUSPENDING one. The grantee's socket was frozen while
// they were suspended, so restoring the grant leaves their map dark until they
// happen to reconnect.
func TestWidenProducerE2E_UnsuspendReHandshakesTheGrantee(t *testing.T) {
	rig := newWidenProducerRig(t, "cgrantee")
	h := telemetry.NewShareInviteHandler(
		&fakeWidenTokens{userID: "cowner"},
		nil, // ServePatch reads no vehicle
		&fakeWidenInviteStore{granteeID: "cgrantee", vehicleID: "veh-gained"},
		nil,
		nil, // no link signer: the patch response carries no shareUrl
		testLogger(),
		shareInviteWidenOption(rig.deps),
	)
	rig.mux.HandleFunc("PATCH /api/invites/{inviteId}", h.ServePatch)

	start := time.Now()
	rig.call(t, http.MethodPatch, "/api/invites/inv_widen_1", `{"suspended":false}`)

	latency := awaitClose4002(t, rig.conn, start, "the un-suspended grantee")
	t.Logf("un-suspend → live socket re-handshaked in %s", latency)
}

// §7.24 RIDE JOIN. The 200 sends the joiner straight to the rider tracking
// surface, holding a session that cannot subscribe to the ride's car.
func TestWidenProducerE2E_RideJoinReHandshakesTheJoiner(t *testing.T) {
	rig := newWidenProducerRig(t, "cjoiner")
	h := telemetry.NewRideRequestHandler(
		&fakeWidenTokens{userID: "cjoiner"},
		nil, // ServeJoin reads no vehicle
		&fakeWidenRideStore{vehicleID: "veh-gained"},
		nil, // no publisher: the ride.member_joined fan-out is not what is under test
		testLogger(),
		rideJoinWidenOption(rig.deps),
	)
	rig.mux.HandleFunc("POST /api/ride-requests/join", h.ServeJoin)

	start := time.Now()
	rig.call(t, http.MethodPost, "/api/ride-requests/join", `{"code":"RBO246"}`)

	latency := awaitClose4002(t, rig.conn, start, "the joining member")
	t.Logf("ride join → live socket re-handshaked in %s", latency)
}
