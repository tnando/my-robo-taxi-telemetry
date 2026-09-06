package store_test

// MYR-599 — the go_vehicle_driver_access WRITE half, against a real database.
//
// The read half is covered by the catalog and snapshot tests; what needs its
// own file is the WRITING, because every statement here is either opening or
// shutting a gate that protects a person who is not our user, and each has a
// failure mode that is silent rather than loud:
//
//   - a row that is not written provisions a car with the gate WIDE OPEN;
//   - a row whose acknowledgment is re-dated destroys the one fact the record
//     exists to hold;
//   - a scope predicate that does not bind files a consent against the wrong
//     person entirely.
//
// Nothing here asserts "the function returned nil". Every case asserts what is
// in the table afterwards.

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

const (
	driverAccessUser  = "cuser_drv"
	driverAccessOther = "cuser_other"
)

// setupDriverAccess gives each test a clean table plus the Prisma rows the
// VIN-keyed statements resolve through.
func setupDriverAccess(t *testing.T) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping driver-access integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM go_vehicle_driver_access`); err != nil {
		t.Fatalf("clean go_vehicle_driver_access: %v", err)
	}
}

// readDriverAccessRow returns the row's ACKNOWLEDGMENT columns, or ok=false when
// there is no row. tesla_access_type is deliberately not returned: what that
// column holds is a property of the write in owner_vehicle_driver_gate.go and is
// asserted by that file's tests, against the statement that actually writes it.
func readDriverAccessRow(t *testing.T, vehicleID string) (ackAt *time.Time, version *string, ok bool) {
	t.Helper()
	err := testPool.QueryRow(context.Background(), `
		SELECT acknowledged_at, acknowledgment_version
		FROM go_vehicle_driver_access WHERE vehicle_id = $1`,
		vehicleID).Scan(&ackAt, &version)
	if err != nil {
		return nil, nil, false
	}
	return ackAt, version, true
}

// seedDriverAccessRow files a driver-access row directly.
//
// RAW SQL RATHER THAN A STORE METHOD, deliberately. The only production writer
// of this row is applyDriverAccess, inside the provisioning transaction, and
// driving it from here would mean seeding a whole "Vehicle" upsert to set up a
// test about the acknowledgment. The VIN-keyed RecordDriverAccess that used to
// serve this purpose was deleted with the rest of the pre-transaction design —
// keeping a second spelling of a consent write alive as a test fixture is
// exactly how a later change picks the wrong one.
//
// What applyDriverAccess actually writes is pinned by its own tests in
// owner_vehicle_driver_gate_test.go, which go through UpsertOwnedVehicle.
func seedDriverAccessRow(t *testing.T, vehicleID, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO go_vehicle_driver_access (vehicle_id, user_id, tesla_access_type)
		VALUES ($1, $2, 'DRIVER')`, vehicleID, userID); err != nil {
		t.Fatalf("seed driver-access row for %s: %v", vehicleID, err)
	}
}

// THE FIRST ACKNOWLEDGMENT WINS. This is the property the `AND acknowledged_at
// IS NULL` predicate exists for, and it is the opposite of this package's usual
// last-write-wins: the instant a person FIRST agreed is what the platform would
// point to, so a lost response, a retry or a second tap must not re-date a
// months-old agreement as today's.
func TestAcknowledgeOwnerApprovalFirstWriteWins(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	first := time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)
	second := time.Date(2026, 12, 25, 8, 0, 0, 0, time.UTC)

	seedVehicleForOwner(t, testPool, "cveh_d5", "5YJ3E1EA1NF000605", driverAccessUser)
	seedDriverAccessRow(t, "cveh_d5", driverAccessUser)

	recorded, err := repo.AcknowledgeOwnerApproval(ctx, "cveh_d5", driverAccessUser, "owner-approval-v1", first)
	if err != nil {
		t.Fatalf("AcknowledgeOwnerApproval: %v", err)
	}
	if !recorded {
		t.Fatal("recorded = false on the FIRST acknowledgment of an unacknowledged driver row")
	}

	// The repeat. It must report "nothing recorded" and change nothing.
	recorded, err = repo.AcknowledgeOwnerApproval(ctx, "cveh_d5", driverAccessUser, "owner-approval-v2", second)
	if err != nil {
		t.Fatalf("AcknowledgeOwnerApproval (repeat): %v", err)
	}
	if recorded {
		t.Error("recorded = true on a REPEAT — the handler would write a second audit row for one consent")
	}

	ackAt, version, ok := readDriverAccessRow(t, "cveh_d5")
	if !ok {
		t.Fatal("the row vanished")
	}
	if ackAt == nil || !ackAt.UTC().Equal(first) {
		t.Errorf("acknowledged_at = %v, want the FIRST instant %v — the record was re-dated", ackAt, first)
	}
	if version == nil || *version != "owner-approval-v1" {
		t.Errorf("acknowledgment_version = %v, want the first version — the record was overwritten", version)
	}

	// EXACTLY ONE audit row for one consent. A second would misrepresent one
	// agreement as two in the trail this feature exists to produce.
	var audits int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM "AuditLog" WHERE "action" = $1 AND "targetId" = $2`,
		string(store.AuditActionOwnerApprovalAcknowledged), "cveh_d5").Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 1 {
		t.Errorf("audit rows = %d, want exactly 1 for one consent", audits)
	}
}

// AN OWNER-ACCESS CAR RECORDS NOTHING AT ALL — no stamp and, critically, NO
// AUDIT ROW. An audit trail that logged non-events would be worse than useless
// in the one conversation it exists for.
func TestAcknowledgeOwnerApprovalOnAnOwnerCarWritesNothing(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	seedVehicleForOwner(t, testPool, "cveh_d6", "5YJ3E1EA1NF000606", driverAccessUser)

	recorded, err := repo.AcknowledgeOwnerApproval(
		ctx, "cveh_d6", driverAccessUser, "owner-approval-v1", time.Now().UTC())
	if err != nil {
		t.Fatalf("AcknowledgeOwnerApproval on an owner car = %v, want nil — it is a fact about the car, not a fault", err)
	}
	if recorded {
		t.Error("recorded = true for a car with no driver-access row")
	}

	var audits int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM "AuditLog" WHERE "action" = $1 AND "targetId" = $2`,
		string(store.AuditActionOwnerApprovalAcknowledged), "cveh_d6").Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 0 {
		t.Errorf("audit rows = %d, want 0 — nothing was acknowledged", audits)
	}
}

// The VIN-keyed gate the fleet-config push route consults. Its three answers
// must be distinguishable, because two of them permit a push and one forbids it.
func TestPendingDriverAcknowledgmentByVIN(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	now := time.Date(2026, 9, 5, 10, 4, 0, 0, time.UTC)

	// An owner's car: no row.
	seedVehicleForOwner(t, testPool, "cveh_d9", "5YJ3E1EA1NF000609", driverAccessUser)
	// A driver's car, unacknowledged: the gate is SHUT.
	seedVehicleForOwner(t, testPool, "cveh_d10", "5YJ3E1EA1NF000610", driverAccessUser)
	seedDriverAccessRow(t, "cveh_d10", driverAccessUser)
	// A driver's car, acknowledged: the gate is OPEN.
	seedVehicleForOwner(t, testPool, "cveh_d11", "5YJ3E1EA1NF000611", driverAccessUser)
	seedDriverAccessRow(t, "cveh_d11", driverAccessUser)
	if _, err := repo.AcknowledgeOwnerApproval(ctx, "cveh_d11", driverAccessUser, "owner-approval-v1", now); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	cases := []struct {
		name        string
		vin         string
		wantPending bool
	}{
		{"owner car — nothing to acknowledge", "5YJ3E1EA1NF000609", false},
		{"driver car, unacknowledged — the gate is shut", "5YJ3E1EA1NF000610", true},
		{"driver car, acknowledged — the gate is open", "5YJ3E1EA1NF000611", false},
		{"a VIN we hold no vehicle for", "5YJ3E1EA1NF000999", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pending, err := repo.PendingDriverAcknowledgmentByVIN(ctx, tc.vin)
			if err != nil {
				t.Fatalf("PendingDriverAcknowledgmentByVIN: %v", err)
			}
			if pending != tc.wantPending {
				t.Errorf("pending = %v, want %v", pending, tc.wantPending)
			}
		})
	}
}

// THE `user_id = $4` GUARD ON THE ACKNOWLEDGMENT STATEMENT.
//
// The §7.29 handler establishes ownership before it calls, so this predicate is
// pure defence — which is exactly why it needs a test of its own. Defence that
// lives only in the caller is defence the SECOND caller will not have, and the
// thing it protects is a consent record: a stamp written against a car the
// acknowledging account does not hold would be the platform asserting that a
// named person agreed to something about somebody else's vehicle.
func TestAcknowledgeOwnerApprovalIsUserScoped(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	seedVehicleForOwner(t, testPool, "cveh_d12", "5YJ3E1EA1NF000612", driverAccessUser)
	seedDriverAccessRow(t, "cveh_d12", driverAccessUser)
	// The other person's own driver car, so the assertion below is about the
	// PREDICATE and not merely about a statement that happened to match nothing:
	// this account really does hold a row of its own, just not this car's.
	seedVehicleForOwner(t, testPool, "cveh_d13", "5YJ3E1EA1NF000613", driverAccessOther)
	seedDriverAccessRow(t, "cveh_d13", driverAccessOther)

	recorded, err := repo.AcknowledgeOwnerApproval(
		ctx, "cveh_d12", driverAccessOther, "owner-approval-v1", time.Now().UTC())
	if err != nil {
		t.Fatalf("AcknowledgeOwnerApproval = %v, want nil — a non-match is zero rows, not a fault", err)
	}
	if recorded {
		t.Fatal("recorded = true for an account that does not hold this car's driver-access row")
	}
	if ackAt, _, ok := readDriverAccessRow(t, "cveh_d12"); !ok || ackAt != nil {
		t.Errorf("acknowledged_at = %v, want it still NULL — the gate was opened by the wrong person", ackAt)
	}
	if ackAt, _, ok := readDriverAccessRow(t, "cveh_d13"); !ok || ackAt != nil {
		t.Errorf("acknowledged_at on the OTHER car = %v, want NULL — the statement is keyed on "+
			"vehicle_id and must not wander to a row this account does hold", ackAt)
	}

	var audits int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM "AuditLog" WHERE "action" = $1 AND "targetId" = $2`,
		string(store.AuditActionOwnerApprovalAcknowledged), "cveh_d12").Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 0 {
		t.Errorf("audit rows = %d, want 0 — nothing was acknowledged", audits)
	}
}
