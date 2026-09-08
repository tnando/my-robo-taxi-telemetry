package main

import (
	"context"
	"log/slog"
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
type recordingWidener struct {
	inv    *recordingInvalidator
	calls  []accessCall
	busted [][]string
}

type accessCall struct{ user, vehicle, reason string }

func (r *recordingWidener) ShareAccessWidened(userID, vehicleID, reason string) {
	r.calls = append(r.calls, accessCall{userID, vehicleID, reason})
	if r.inv != nil {
		r.busted = append(r.busted, append([]string(nil), r.inv.users...))
	}
}

// recordingNarrower is the transfer's other end.
type recordingNarrower struct{ calls []accessCall }

func (r *recordingNarrower) ShareAccessRevoked(userID, vehicleID, reason string) {
	r.calls = append(r.calls, accessCall{userID, vehicleID, reason})
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
	inv := &recordingInvalidator{}
	wid := &recordingWidener{inv: inv}
	nar := &recordingNarrower{}
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		&fakeUpserter{outcome: store.VehicleOwnedByTransfer, previousUserID: "cdriver"},
		ownerStreamAccess{invalidator: inv, widener: wid, narrower: nar},
	)

	hook.AfterLink(context.Background(), "cowner", "token")

	if len(wid.calls) != 1 || wid.calls[0].user != "cowner" || wid.calls[0].reason != "owner_transfer" {
		t.Errorf("widenings = %+v, want one {cowner … owner_transfer}", wid.calls)
	}
	if len(nar.calls) != 1 || nar.calls[0].user != "cdriver" || nar.calls[0].reason != "superseded_by_owner" {
		t.Errorf("narrowings = %+v, want one {cdriver … superseded_by_owner} — the former "+
			"driver keeps the car's live GPS until something tells the hub otherwise", nar.calls)
	}
	// BOTH accounts' cached sets are busted, and the transfer is not `Inserted`,
	// so this also pins that the transfer takes its own branch.
	if len(inv.users) != 2 {
		t.Errorf("cache busts = %v, want both the owner's and the former driver's", inv.users)
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
	hook := newAccessHook(t,
		[]telemetry.FleetVehicle{ownedVehicle("111", accessVIN, "Lunar")},
		upsert,
		ownerStreamAccess{},
	)

	hook.AfterLink(context.Background(), "cuser1", "token")

	if len(upsert.inputs) != 1 {
		t.Fatalf("the link itself did not complete: upserts = %+v", upsert.inputs)
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
