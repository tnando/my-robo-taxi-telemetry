package store_test

// MYR-599 OWNER WINS — the cross-user conflict rules, against a real database.
//
// These are the two directions of one rule, and they are asserted separately
// because they are not symmetric and a future reader will assume they are:
//
//   - an OWNER link onto a DRIVER-provisioned row TRANSFERS the car, and drags
//     the previous linker's gate, schedule and shares with it;
//   - a DRIVER link onto a row an OWNER holds does NOTHING, exactly as before.
//
// The transfer's CLEANUP is asserted item by item rather than through the
// outcome enum, because every one of those rows is an assertion about a car the
// former linker does not own and each would survive silently if it were dropped.
// The share revocation is the one that would actually hurt: a new owner
// inheriting a car a stranger's contacts can watch, with no UI anywhere that
// would ever show them why.

import (
	"context"
	"sort"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

const (
	transferDriver = "cxfer_driver01"
	transferOwner  = "cxfer_owner001"
	transferVID    = "vid-xfer-1"
	transferVIN    = "5YJ3E1EA7KF000601"
	// The driver's own viewers — third parties whose access the transfer
	// revokes in the same statement (MYR-601).
	transferViewerA = "cxfer_viewer_a"
	transferViewerB = "cxfer_viewer_b"
)

// seedDriverProvisionedCar builds the state a driver's AfterLink leaves behind:
// a "Vehicle" row filed under them, an unacknowledged consent gate, a
// fleet-config schedule row carrying `awaiting_owner_ack`, and a live share.
func seedDriverProvisionedCar(t *testing.T, vehicleID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO "Vehicle" ("id","userId","teslaVehicleId","vin","name","updatedAt")
		 VALUES ($1, $2, $3, $4, 'Borrowed', NOW())`,
		vehicleID, transferDriver, transferVID, transferVIN); err != nil {
		t.Fatalf("seed driver-provisioned vehicle: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_vehicle_driver_access (vehicle_id, user_id, tesla_access_type)
		 VALUES ($1, $2, 'DRIVER')`, vehicleID, transferDriver); err != nil {
		t.Fatalf("seed driver access: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_fleet_config_attempts (vehicle_id, last_outcome) VALUES ($1, $2)`,
		vehicleID, store.SetupOutcomeAwaitingOwnerAck); err != nil {
		t.Fatalf("seed fleet-config schedule: %v", err)
	}
	// TWO REDEEMED VIEWERS AND ONE UNREDEEMED INVITE (MYR-601). The viewers are
	// the people the transfer's teardown cuts who never linked anything — they
	// are simply watching a car somebody shared with them, and their sessions
	// are the ones most likely to be open. The pending row carries a NULL
	// grantee and must not reach the caller as one.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_vehicle_shares
			(id, vehicle_id, owner_user_id, accepted_by_user_id, label, permission, code, status, expires_at)
		 VALUES
			('shr_xfer',   $1, $2, $3,   'A friend',   'rides',  'XFERCODE', 'accepted', NOW() + INTERVAL '7 days'),
			('shr_xfer_2', $1, $2, $4,   'Another',    'live',   'XFERCOD2', 'accepted', NOW() + INTERVAL '7 days'),
			('shr_xfer_3', $1, $2, NULL, 'Not yet in', 'live',   'XFERCOD3', 'pending',  NOW() + INTERVAL '7 days')`,
		vehicleID, transferDriver, transferViewerA, transferViewerB); err != nil {
		t.Fatalf("seed shares: %v", err)
	}
}

func TestOwnerProvisioner_OwnerWinsTransfer(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping owner-wins transfer integration test")
	}
	ctx := context.Background()
	ensureOwnerSchema(t)
	mustApplyGoMigrations(t)
	if _, err := testPool.Exec(ctx, teardownSchemaSQL); err != nil {
		t.Fatalf("apply teardown schema: %v", err)
	}
	prov := newTestProvisioner(t)

	t.Run("an OWNER link takes a driver-provisioned car and clears the driver's claim", func(t *testing.T) {
		cleanOwnerTables(t)
		cleanTransferTables(t)
		seedOwnerUser(t, transferDriver, "", "")
		seedOwnerUser(t, transferOwner, "", "")
		seedOwnerUser(t, transferViewerA, "", "")
		seedOwnerUser(t, transferViewerB, "", "")
		const vehicleID = "veh_xfer_1"
		seedDriverProvisionedCar(t, vehicleID)

		out, err := prov.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{
			UserID:          transferOwner,
			TeslaVehicleID:  transferVID,
			VIN:             transferVIN,
			Name:            "My car",
			TeslaAccessType: "OWNER",
			Access:          store.AccessSignalOwner,
		})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if out.Outcome != store.VehicleOwnedByTransfer {
			t.Fatalf("outcome = %q, want owned_by_transfer — without it the real owner's own "+
				"car never appears in their app and nothing in the system can fix it",
				out.Outcome)
		}
		if out.VehicleID != vehicleID {
			t.Errorf("vehicle id = %q, want the EXISTING row %q — a transfer moves a row, "+
				"it does not create a second one", out.VehicleID, vehicleID)
		}
		// MYR-601: the transfer is TWO access-set changes, and the second one is
		// only actionable if the caller is told whose it was. The former driver's
		// cached set has to be busted and their live sockets closed — the same
		// account this transaction just revoked every share of — and this is the
		// only place that id is known.
		if out.PreviousUserID != transferDriver {
			t.Errorf("previous user = %q, want the FORMER DRIVER %q — without it their cached "+
				"access set stays warm for the TTL and their open socket keeps streaming the "+
				"car's live GPS", out.PreviousUserID, transferDriver)
		}
		// AND EVERY THIRD PARTY THE SAME STATEMENT CUT (MYR-601). The linker is
		// not the only loser: `queryRevokeSharesForVehicle` tombstones every
		// live grant on the car, so the driver's viewers lose access too — and
		// nothing else in the system would ever tell the hub about them.
		if got := sortedCopy(out.RevokedGranteeIDs); len(got) != 2 ||
			got[0] != transferViewerA || got[1] != transferViewerB {
			t.Errorf("revoked grantees = %v, want both redeemed viewers [%s %s] and NOT the "+
				"pending invite's NULL — a viewer nobody names keeps the car's live GPS "+
				"until the cache TTL and the sweep catch up",
				out.RevokedGranteeIDs, transferViewerA, transferViewerB)
		}

		var owner, name string
		if err := testPool.QueryRow(ctx,
			`SELECT "userId", "name" FROM "Vehicle" WHERE "id" = $1`, vehicleID).Scan(&owner, &name); err != nil {
			t.Fatalf("read vehicle: %v", err)
		}
		if owner != transferOwner {
			t.Errorf("vehicle owner = %q, want %q", owner, transferOwner)
		}
		if name != "Borrowed" {
			t.Errorf("name = %q, want the existing name preserved — the owner is receiving a "+
				"real row with history, not a fresh provision", name)
		}

		if n := countQuery(t, `SELECT count(*) FROM go_vehicle_driver_access WHERE vehicle_id = $1`, vehicleID); n != 0 {
			t.Errorf("driver-access rows = %d, want 0 — a surviving gate would keep the wire "+
				"saying teslaAccessType:driver about a car this person owns outright, and "+
				"(unacknowledged) hold every push path shut against them", n)
		}
		if n := countQuery(t, `SELECT count(*) FROM go_fleet_config_attempts WHERE vehicle_id = $1`, vehicleID); n != 0 {
			t.Errorf("schedule rows = %d, want 0 — a surviving awaiting_owner_ack additionally "+
				"exempts the car from the MYR-592 sweeper forever", n)
		}
		var shareStatus string
		if err := testPool.QueryRow(ctx,
			`SELECT status FROM go_vehicle_shares WHERE id = 'shr_xfer'`).Scan(&shareStatus); err != nil {
			t.Fatalf("read share: %v", err)
		}
		if shareStatus != "revoked" {
			t.Errorf("share status = %q, want revoked — otherwise the new owner inherits a car "+
				"a stranger's contacts can watch, with no UI that would ever show them why",
				shareStatus)
		}

		if n := countQuery(t,
			`SELECT count(*) FROM "AuditLog" WHERE "action" = $1 AND "targetId" = $2 AND "userId" = $3`,
			string(store.AuditActionDriverLinkSupersededByOwner), vehicleID, transferDriver); n != 1 {
			t.Errorf("audit rows filed under the FORMER DRIVER = %d, want 1 — it is the only "+
				"record they have of where their car went", n)
		}
	})

	t.Run("a DRIVER link onto an OWNER's car is still a plain cross-user skip", func(t *testing.T) {
		cleanOwnerTables(t)
		cleanTransferTables(t)
		seedOwnerUser(t, transferDriver, "", "")
		seedOwnerUser(t, transferOwner, "", "")
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "Vehicle" ("id","userId","teslaVehicleId","vin","name","updatedAt")
			 VALUES ('veh_xfer_2', $1, $2, $3, 'Mine', NOW())`,
			transferOwner, transferVID, transferVIN); err != nil {
			t.Fatalf("seed owner vehicle: %v", err)
		}

		out, err := prov.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{
			UserID:          transferDriver,
			TeslaVehicleID:  transferVID,
			VIN:             transferVIN,
			TeslaAccessType: "DRIVER",
			Access:          store.AccessSignalDriver,
		})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if out.Outcome != store.VehicleSkippedCrossUser {
			t.Fatalf("outcome = %q, want skipped_cross_user — OWNER WINS BOTH WAYS; a driver "+
				"may not take an owner's row, and the owner sharing the car back is the "+
				"documented path", out.Outcome)
		}
		// And a SKIP names nobody (MYR-601). A previous holder — or a grantee —
		// reported here would have the hub close untouched sessions over a link
		// that changed nothing.
		if out.PreviousUserID != "" {
			t.Errorf("previous user = %q on a skip, want empty", out.PreviousUserID)
		}
		if len(out.RevokedGranteeIDs) != 0 {
			t.Errorf("revoked grantees = %v on a skip, want none", out.RevokedGranteeIDs)
		}
		var owner string
		if err := testPool.QueryRow(ctx,
			`SELECT "userId" FROM "Vehicle" WHERE "id" = 'veh_xfer_2'`).Scan(&owner); err != nil {
			t.Fatalf("read vehicle: %v", err)
		}
		if owner != transferOwner {
			t.Errorf("vehicle owner = %q, want %q untouched", owner, transferOwner)
		}
		if n := countQuery(t, `SELECT count(*) FROM go_vehicle_driver_access`); n != 0 {
			t.Errorf("driver-access rows = %d, want 0 — nothing was provisioned, so nothing "+
				"should have been gated", n)
		}
	})

	t.Run("an OWNER-versus-OWNER collision is NOT transferred", func(t *testing.T) {
		cleanOwnerTables(t)
		cleanTransferTables(t)
		seedOwnerUser(t, transferDriver, "", "")
		seedOwnerUser(t, transferOwner, "", "")
		// Same seed as the transfer case MINUS the driver-access row: an
		// ordinary cross-user conflict between two owner-provisioned rows.
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "Vehicle" ("id","userId","teslaVehicleId","vin","name","updatedAt")
			 VALUES ('veh_xfer_3', $1, $2, $3, 'Theirs', NOW())`,
			transferDriver, transferVID, transferVIN); err != nil {
			t.Fatalf("seed vehicle: %v", err)
		}

		out, err := prov.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{
			UserID:          transferOwner,
			TeslaVehicleID:  transferVID,
			VIN:             transferVIN,
			TeslaAccessType: "OWNER",
			Access:          store.AccessSignalOwner,
		})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if out.Outcome != store.VehicleSkippedCrossUser {
			t.Fatalf("outcome = %q, want skipped_cross_user — the transfer is keyed on the "+
				"DRIVER-ACCESS ROW, a state only MYR-599 can create. Two owners colliding on "+
				"one teslaVehicleId is a data problem, and resolving it by moving somebody's "+
				"car would be a guess", out.Outcome)
		}
		var owner string
		if err := testPool.QueryRow(ctx,
			`SELECT "userId" FROM "Vehicle" WHERE "id" = 'veh_xfer_3'`).Scan(&owner); err != nil {
			t.Fatalf("read vehicle: %v", err)
		}
		if owner != transferDriver {
			t.Errorf("vehicle owner = %q, want %q untouched", owner, transferDriver)
		}
	})
}

func cleanTransferTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{
		"go_vehicle_driver_access",
		"go_fleet_config_attempts",
		"go_vehicle_shares",
		// "AuditLog" is deliberately NOT cleaned: the table carries an
		// append-only trigger, which is the point of an audit trail. Every
		// assertion below scopes by targetId instead, and each subtest uses its
		// own vehicle id.
		`"Vehicle"`,
	} {
		if _, err := testPool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
}

// sortedCopy orders a returned id list so an assertion does not depend on the
// order Postgres happened to return the tombstoned rows in.
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
