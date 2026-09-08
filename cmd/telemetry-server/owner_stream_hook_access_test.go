package main

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// MYR-601 — provisioning a car is an access-set WIDENING, and until this it was
// the one widening producer that announced nothing.
//
// The end-to-end cases run through the REAL bus and a REAL WebSocket client,
// like share_widen_dispatcher_test.go next door and for the same reason: what
// was broken was not any one struct — the hub, the cache and the link path were
// all individually correct — it was that nothing connected them, and only a
// test that spans the whole path can fail on that.

// recordingInvalidator records the cache busts in order, so the bust-before-
// publish rule can be asserted rather than assumed.
type recordingInvalidator struct {
	users []string
}

func (r *recordingInvalidator) InvalidateVehicles(userID string) {
	r.users = append(r.users, userID)
}

// recordingWidener records widenings AND what the invalidator had done first.
//
// `seq` is the SHARED ordering log the narrower writes to as well: the transfer
// publishes in both directions and the order between them is normative (losses
// first — see announceProvisioned), which no per-recorder list can show.
type recordingWidener struct {
	inv    *recordingInvalidator
	seq    *[]string
	calls  []accessCall
	busted [][]string
}

type accessCall struct{ user, vehicle, reason string }

func (r *recordingWidener) ShareAccessWidened(userID, vehicleID, reason string) {
	r.calls = append(r.calls, accessCall{userID, vehicleID, reason})
	if r.inv != nil {
		r.busted = append(r.busted, append([]string(nil), r.inv.users...))
	}
	if r.seq != nil {
		*r.seq = append(*r.seq, "gained:"+userID)
	}
}

// recordingNarrower is the transfer's other end.
type recordingNarrower struct {
	seq   *[]string
	calls []accessCall
}

func (r *recordingNarrower) ShareAccessRevoked(userID, vehicleID, reason string) {
	r.calls = append(r.calls, accessCall{userID, vehicleID, reason})
	if r.seq != nil {
		*r.seq = append(*r.seq, "lost:"+userID)
	}
}

const accessVIN = "5YJ3E1EA7KF000042"

// newAccessHook builds a hook whose only wired dependency is the access seam.
func newAccessHook(t *testing.T, fleet []telemetry.FleetVehicle, upsert *fakeUpserter,
	access ownerStreamAccess) *ownerStreamHook {
	t.Helper()
	return &ownerStreamHook{
		lister: &fakeLister{vehicles: fleet},
		upsert: upsert,
		access: access,
		logger: testLogger(),
	}
}

// THE HEADLINE. A car that was not there a moment ago busts the linker's cached
// access set and publishes the widening, in that order. Everything else in this
// file is a boundary on this one behavior.
func TestOwnerStreamHook_NewCarWidensTheAccessSet(t *testing.T) {
	inv := &recordingInvalidator{}
	wid := &recordingWidener{inv: inv}
	upsert := &fakeUpserter{inserted: true}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		upsert,
		ownerStreamAccess{invalidator: inv, widener: wid},
	)

	hook.AfterLink(context.Background(), "cuser1", "token")

	if got := inv.users; len(got) != 1 || got[0] != "cuser1" {
		t.Fatalf("cache busts = %v, want exactly [cuser1] — without this the next "+
			"handshake is served the PRE-LINK set from memory for up to the TTL", got)
	}
	if len(wid.calls) != 1 {
		t.Fatalf("widenings = %+v, want exactly one", wid.calls)
	}
	got := wid.calls[0]
	if got.user != "cuser1" || got.vehicle != "veh_"+accessVIN || got.reason != "provisioned" {
		t.Errorf("widening = %+v, want {cuser1 veh_%s provisioned}", got, accessVIN)
	}
	// ORDER IS THE CORRECTNESS. A widen that overtook the bust would provoke a
	// reconnect served from the stale cached set — a fix that runs, logs, and
	// leaves the car missing.
	if len(wid.busted) != 1 || len(wid.busted[0]) != 1 {
		t.Errorf("the widening was published before the cache bust (busts seen at publish: %v)", wid.busted)
	}
}

// A DRIVER-ACCESS CAR WIDENS TOO, and this is the case MYR-601 was actually
// reported from. The consent gate holds the Tesla-side PUSH, not the row: the
// car is in its linker's access set the moment it is provisioned, §7.0 lists it,
// and their app must be able to subscribe to it. Announcing only for owner cars
// would leave exactly the reported bug in place for exactly the reported car.
func TestOwnerStreamHook_UnacknowledgedDriverCarStillWidens(t *testing.T) {
	inv := &recordingInvalidator{}
	wid := &recordingWidener{inv: inv}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{driverVehicle("222", accessVIN, "Borrowed")},
		&fakeUpserter{inserted: true},
		ownerStreamAccess{invalidator: inv, widener: wid},
	)

	hook.AfterLink(context.Background(), "cdriver", "token")

	if len(wid.calls) != 1 || wid.calls[0].user != "cdriver" {
		t.Fatalf("widenings = %+v, want one for cdriver — an unacknowledged driver car "+
			"is in its linker's access set exactly like an owner's", wid.calls)
	}
}

// AND THE BOUNDARY THAT KEEPS IT AFFORDABLE. AfterLink is a passive bulk sync
// over the owner's whole fleet, run on every link and re-link. A car that was
// merely reconciled has been in the access set all along, so announcing it
// would re-handshake every session the owner holds each time they refresh their
// Tesla consent — a disconnect storm bought with nothing.
func TestOwnerStreamHook_ReconciledCarAnnouncesNothing(t *testing.T) {
	inv := &recordingInvalidator{}
	wid := &recordingWidener{inv: inv}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		&fakeUpserter{inserted: false},
		ownerStreamAccess{invalidator: inv, widener: wid},
	)

	hook.AfterLink(context.Background(), "cuser1", "token")

	if len(inv.users) != 0 || len(wid.calls) != 0 {
		t.Errorf("a re-link of a car already on the account announced %v / %+v, want nothing",
			inv.users, wid.calls)
	}
}

// The skip outcomes never reached an access set at all: the tombstone gate
// refused to resurrect a removed car, and the cross-user gate refused to
// reassign somebody else's. Announcing either would tell a user they gained a
// car the database just declined to give them.
func TestOwnerStreamHook_SkippedProvisionsAnnounceNothing(t *testing.T) {
	tests := []struct {
		name    string
		outcome store.VehicleUpsertOutcome
	}{
		{"the owner deliberately removed this car (MYR-261 tombstone)", store.VehicleSkippedTombstoned},
		{"the car belongs to somebody this link does not outrank", store.VehicleSkippedCrossUser},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := &recordingInvalidator{}
			wid := &recordingWidener{inv: inv}
			hook := newAccessHook(t,
				[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
				// `inserted` is deliberately true: the skip must win over it,
				// because a fake that agreed with the branch would prove nothing.
				&fakeUpserter{outcome: tt.outcome, inserted: true},
				ownerStreamAccess{invalidator: inv, widener: wid},
			)

			hook.AfterLink(context.Background(), "cuser1", "token")

			if len(inv.users) != 0 || len(wid.calls) != 0 {
				t.Errorf("a skipped provision announced %v / %+v, want nothing", inv.users, wid.calls)
			}
		})
	}
}

// THE MYR-599 TRANSFER IS TWO ACCESS-SET CHANGES AND BOTH ARE ANNOUNCED. The
// arriving owner gains the car; the former driver loses it, along with every
// share they had issued on it — and the narrowing is the half that matters
// most, because a late one leaves somebody streaming live GPS from a car that
// is no longer theirs in any sense.
func TestOwnerStreamHook_TransferAnnouncesBothEnds(t *testing.T) {
	var seq []string
	inv := &recordingInvalidator{}
	wid := &recordingWidener{inv: inv, seq: &seq}
	nar := &recordingNarrower{seq: &seq}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		&fakeUpserter{
			outcome:           store.VehicleOwnedByTransfer,
			previousUserID:    "cdriver",
			revokedGranteeIDs: []string{"cviewer_a", "cviewer_b"},
		},
		ownerStreamAccess{invalidator: inv, widener: wid, narrower: nar},
	)

	hook.AfterLink(context.Background(), "cowner", "token")

	if len(wid.calls) != 1 || wid.calls[0].user != "cowner" || wid.calls[0].reason != "owner_transfer" {
		t.Errorf("widenings = %+v, want one {cowner … owner_transfer}", wid.calls)
	}
	// THREE narrowings, and each carries the reason for ITS kind of loss: the
	// linker lost a car they had linked, the viewers lost a share somebody else
	// had given them, and an audit trail should be able to tell them apart.
	wantNarrow := []accessCall{
		{"cdriver", "veh_" + accessVIN, "superseded_by_owner"},
		{"cviewer_a", "veh_" + accessVIN, "share_superseded_by_owner"},
		{"cviewer_b", "veh_" + accessVIN, "share_superseded_by_owner"},
	}
	if !reflect.DeepEqual(nar.calls, wantNarrow) {
		t.Errorf("narrowings = %+v, want %+v — every account the teardown cut keeps the "+
			"car's live GPS until something tells the hub otherwise", nar.calls, wantNarrow)
	}
	// EVERY loser's cached set is busted, plus the arriving owner's. The
	// transfer is not `Inserted`, so this also pins that it takes its own branch.
	if len(inv.users) != 4 {
		t.Errorf("cache busts = %v, want the owner's and all three losers'", inv.users)
	}
	// AND THE LOSSES ARE PUBLISHED FIRST (MYR-601 review finding 8). Both go
	// onto an in-process bus that drops the OLDEST event when a subscriber is
	// behind; if exactly one is going to be lost it must not be the one whose
	// latency is a stranger's live GPS.
	wantSeq := []string{"lost:cdriver", "lost:cviewer_a", "lost:cviewer_b", "gained:cowner"}
	if !reflect.DeepEqual(seq, wantSeq) {
		t.Errorf("publish order = %v, want %v — the security half goes first", seq, wantSeq)
	}
}

// A transfer whose previous holder came back empty announces only the gain.
// The narrowing's user id is not a wildcard, and reaching the hub as one would
// disconnect every connected client on the process.
func TestOwnerStreamHook_TransferWithNoPreviousHolderNarrowsNobody(t *testing.T) {
	nar := &recordingNarrower{}
	wid := &recordingWidener{}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		&fakeUpserter{outcome: store.VehicleOwnedByTransfer},
		ownerStreamAccess{widener: wid, narrower: nar},
	)

	hook.AfterLink(context.Background(), "cowner", "token")

	if len(nar.calls) != 0 {
		t.Errorf("narrowings = %+v, want none", nar.calls)
	}
	if len(wid.calls) != 1 {
		t.Errorf("widenings = %+v, want the gain to still be announced", wid.calls)
	}
}

// EVERY FIELD OF THE SEAM IS OPTIONAL, and the whole-zero-value case is what
// dev mode and every test that wires no bus get. It must degrade to the
// pre-MYR-601 behavior — the car appears at the TTL lapse — rather than panic
// on the link path.
func TestOwnerStreamHook_NilAccessSeamIsAQuietNoOp(t *testing.T) {
	upsert := &fakeUpserter{inserted: true}
	// A REAL log sink, because "quiet" is half the claim: with nothing wired
	// there is no announcement to make, and an `owner_vehicle_access_widened`
	// line would be the audit trail asserting one that never left the process.
	var logged bytes.Buffer
	hook := &ownerStreamHook{
		lister: &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")}},
		upsert: upsert,
		access: ownerStreamAccess{},
		logger: slog.New(slog.NewTextHandler(&logged, nil)),
	}

	hook.AfterLink(context.Background(), "cuser1", "token")

	if len(upsert.inputs) != 1 {
		t.Fatalf("the link itself did not complete: upserts = %+v", upsert.inputs)
	}
	if strings.Contains(logged.String(), "owner_vehicle_access_widened") {
		t.Errorf("logged an access-set widening with no invalidator and no seam wired:\n%s",
			logged.String())
	}
}

// END TO END THROUGH THE REAL BUS. The scenario from the issue: the app is
// CONNECTED when the owner links a second car, so its session's `vehicleIDs`
// were frozen before the car existed and no amount of cache correctness can
// help it. Something has to make a next handshake happen.
func TestOwnerStreamHook_NewCarReHandshakesTheOpenSocket(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()
	if _, err := newShareWidenDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Authorized for the three cars they had — never for the fourth.
	srv := newWSTestServer(t, hub, &fakeAuth{userID: "cuser1", vehicleIDs: []string{"veh-a", "veh-b", "veh-c"}})
	defer srv.Close()
	conn := dialWSAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitClients(t, hub, 1)

	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Fourth car")},
		&fakeUpserter{inserted: true},
		ownerStreamAccess{widener: newShareWidenBusNotifier(bus, logger)},
	)

	start := time.Now()
	hook.AfterLink(context.Background(), "cuser1", "token")

	latency := awaitClose4002(t, conn, start, "linking owner")
	if latency > 2*time.Second {
		t.Errorf("the newly linked car took %s to reach the open socket; the issue measured "+
			"about three minutes and the fix is supposed to be immediate", latency)
	}
	t.Logf("link → live socket re-handshaked in %s", latency)
}

// The transfer's narrowing goes onto the REVOCATION topic, not the widening
// one. The two topics are kept apart precisely so anything that watches, counts
// or alerts on revocations does not have conveniences folded into its numbers —
// and a former driver losing a car is a revocation by every measure that counts.
func TestOwnerStreamHook_TransferNarrowingUsesTheRevocationTopic(t *testing.T) {
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

	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		&fakeUpserter{outcome: store.VehicleOwnedByTransfer, previousUserID: "cdriver"},
		ownerStreamAccess{
			widener:  newShareWidenBusNotifier(bus, logger),
			narrower: newShareAccessBusNotifier(bus, logger),
		},
	)
	hook.AfterLink(context.Background(), "cowner", "token")

	select {
	case e := <-revocations:
		payload, ok := e.Payload.(events.ShareAccessRevokedEvent)
		if !ok || payload.GranteeUserID != "cdriver" {
			t.Fatalf("revocation payload = %+v, want the former driver", e.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the former driver's revocation was never published")
	}
	select {
	case e := <-widenings:
		payload, ok := e.Payload.(events.ShareAccessWidenedEvent)
		if !ok || payload.GranteeUserID != "cowner" {
			t.Fatalf("widening payload = %+v, want the arriving owner", e.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the arriving owner's widening was never published")
	}
}

// THE FORMER DRIVER IS NOT THE ONLY LOSER, and the others are the ones most
// likely to be watching. The transfer's teardown tombstones EVERY live grant on
// the car, so the driver's viewers — people who never linked anything — lose
// access in the same statement. Until MYR-601's review round only the driver's
// socket was closed, which handed the arriving owner a car whose live GPS was
// still streaming to two strangers until the cache TTL and the sweep caught up.
//
// End to end through the REAL bus and REAL sockets, because the claim is about
// what reaches a connection, not about which method was called.
func TestOwnerStreamHook_TransferNarrowsEveryRevokedGrantee(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()
	if _, err := newShareAccessDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe revocations: %v", err)
	}
	if _, err := newShareWidenDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe widenings: %v", err)
	}

	const carID = "veh_" + accessVIN
	// The two viewers hold the CAR, which is what makes them findable by a
	// vehicle-scoped revocation — and what makes them a live GPS leak.
	viewerASrv := newWSTestServer(t, hub, &fakeAuth{userID: "cviewer_a", vehicleIDs: []string{carID}})
	defer viewerASrv.Close()
	viewerBSrv := newWSTestServer(t, hub, &fakeAuth{userID: "cviewer_b", vehicleIDs: []string{carID}})
	defer viewerBSrv.Close()
	// And the arriving OWNER, who is connected and must be re-handshaked rather
	// than left without the car they just linked.
	ownerSrv := newWSTestServer(t, hub, &fakeAuth{userID: "cowner", vehicleIDs: []string{"veh-other"}})
	defer ownerSrv.Close()

	viewerA := dialWSAuth(t, viewerASrv.URL, "tok")
	defer viewerA.Close(websocket.StatusNormalClosure, "test done")
	viewerB := dialWSAuth(t, viewerBSrv.URL, "tok")
	defer viewerB.Close(websocket.StatusNormalClosure, "test done")
	ownerConn := dialWSAuth(t, ownerSrv.URL, "tok")
	defer ownerConn.Close(websocket.StatusNormalClosure, "test done")
	waitClients(t, hub, 3)

	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		&fakeUpserter{
			outcome:           store.VehicleOwnedByTransfer,
			previousUserID:    "cdriver",
			revokedGranteeIDs: []string{"cviewer_a", "cviewer_b"},
		},
		ownerStreamAccess{
			widener:  newShareWidenBusNotifier(bus, logger),
			narrower: newShareAccessBusNotifier(bus, logger),
		},
	)

	start := time.Now()
	hook.AfterLink(context.Background(), "cowner", "token")

	awaitClose4002(t, viewerA, start, "a viewer the transfer revoked")
	awaitClose4002(t, viewerB, start, "the second viewer the transfer revoked")
	awaitClose4002(t, ownerConn, start, "the arriving owner")
}

// A FIRST LINK OF AN N-CAR FLEET IS ONE ACCESS-SET CHANGE, NOT N (MYR-601
// review round). The pass announced per car, so a three-car first link closed
// every session that owner held three times — each close provoking a reconnect
// that raced the next one — to deliver a fact the FIRST re-handshake had
// already delivered in full, since the reconnect re-derives the whole access
// set. It is §7.5.5 redeem's one-signal-per-redemption rule from the other side.
func TestOwnerStreamHook_AFleetOfNewCarsAnnouncesOnce(t *testing.T) {
	inv := &recordingInvalidator{}
	wid := &recordingWidener{inv: inv}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{
			ownedVehicle("111", "5YJ3E1EA7KF000042", "Lunar"),
			ownedVehicle("222", "5YJ3E1EA7KF000043", "Solar"),
			ownedVehicle("333", "5YJ3E1EA7KF000044", "Stellar"),
		},
		&fakeUpserter{inserted: true},
		ownerStreamAccess{invalidator: inv, widener: wid},
	)

	hook.AfterLink(context.Background(), "cuser1", "token")

	if len(wid.calls) != 1 {
		t.Fatalf("widenings = %+v, want exactly 1 for a three-car first link — the "+
			"re-handshake re-derives the WHOLE access set, so the second and third "+
			"closes deliver nothing and race the reconnect the first one provoked",
			wid.calls)
	}
	if len(inv.users) != 1 || inv.users[0] != "cuser1" {
		t.Errorf("cache busts = %v, want exactly one for the linking owner", inv.users)
	}
	// It is still ordered: the one bust precedes the one publish.
	if len(wid.busted) != 1 || len(wid.busted[0]) != 1 {
		t.Errorf("the widening was published before the cache bust (busts at publish: %v)", wid.busted)
	}
}

// AND A MIXED PASS STILL ANNOUNCES ONCE, with the losses published in full.
// Every account the transfer cut has its own socket to close — one per person,
// because they are different people — while the arriving owner gains one
// re-handshake however many cars arrived in the same pass.
func TestOwnerStreamHook_MixedFleetAnnouncesOneGainAndEveryLoss(t *testing.T) {
	var seq []string
	inv := &recordingInvalidator{}
	wid := &recordingWidener{inv: inv, seq: &seq}
	nar := &recordingNarrower{seq: &seq}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{
			ownedVehicle("111", "5YJ3E1EA7KF000042", "Lunar"),
			ownedVehicle("222", "5YJ3E1EA7KF000043", "Solar"),
		},
		// Both cars take the TRANSFER branch, so the pass produces two gains
		// worth of evidence and two sets of losers.
		&fakeUpserter{
			outcome:           store.VehicleOwnedByTransfer,
			previousUserID:    "cdriver",
			revokedGranteeIDs: []string{"cviewer_a"},
		},
		ownerStreamAccess{invalidator: inv, widener: wid, narrower: nar},
	)

	hook.AfterLink(context.Background(), "cowner", "token")

	if len(wid.calls) != 1 || wid.calls[0].reason != "owner_transfer" {
		t.Errorf("widenings = %+v, want exactly one {cowner … owner_transfer}", wid.calls)
	}
	if len(nar.calls) != 4 {
		t.Errorf("narrowings = %+v, want four — the driver and the viewer, once per car, "+
			"because each is a different person's socket on a different car", nar.calls)
	}
	// And the single gain is announced LAST, after every loss of the pass.
	if len(seq) == 0 || seq[len(seq)-1] != "gained:cowner" {
		t.Errorf("publish order = %v, want the gain last — a deferred loss is a stranger "+
			"streaming live GPS for as long as the pass runs", seq)
	}
}
