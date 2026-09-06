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

// readDriverAccessRow returns the raw stored row, or ok=false when absent.
func readDriverAccessRow(t *testing.T, vehicleID string) (accessType string, ackAt *time.Time, version *string, ok bool) {
	t.Helper()
	err := testPool.QueryRow(context.Background(), `
		SELECT tesla_access_type, acknowledged_at, acknowledgment_version
		FROM go_vehicle_driver_access WHERE vehicle_id = $1`,
		vehicleID).Scan(&accessType, &ackAt, &version)
	if err != nil {
		return "", nil, nil, false
	}
	return accessType, ackAt, version, true
}

// TestRecordDriverAccess pins what the link-time hook writes, and — more
// importantly — what it refuses to write.
func TestRecordDriverAccess(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	prov := newTestProvisioner(t)
	now := time.Date(2026, 9, 5, 10, 4, 0, 0, time.UTC)

	seedVehicleForOwner(t, testPool, "cveh_d1", "5YJ3E1EA1NF000601", driverAccessUser)

	t.Run("files the row with Tesla's token verbatim", func(t *testing.T) {
		if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000601", driverAccessUser, "DRIVER", now); err != nil {
			t.Fatalf("RecordDriverAccess: %v", err)
		}
		accessType, ackAt, version, ok := readDriverAccessRow(t, "cveh_d1")
		if !ok {
			t.Fatal("no row written — the car is provisioned with the consent gate OPEN")
		}
		if accessType != "DRIVER" {
			t.Errorf("tesla_access_type = %q, want Tesla's own %q", accessType, "DRIVER")
		}
		// NULL on both acknowledgment columns IS the shut gate.
		if ackAt != nil || version != nil {
			t.Errorf("acknowledged_at/version = %v/%v, want NULL/NULL — a fresh row must not be pre-acknowledged",
				ackAt, version)
		}
	})

	// AN EMPTY access_type IS STORED AS '', NOT INVENTED. Older Fleet responses
	// have shipped one, the caller already treats absence as not-owner (fail
	// closed), and writing "DRIVER" here would erase the one thing this column
	// is for: answering, later, what Tesla actually said.
	t.Run("stores an EMPTY access type rather than inventing one", func(t *testing.T) {
		seedVehicleForOwner(t, testPool, "cveh_d2", "5YJ3E1EA1NF000602", driverAccessUser)
		if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000602", driverAccessUser, "", now); err != nil {
			t.Fatalf("RecordDriverAccess: %v", err)
		}
		accessType, _, _, ok := readDriverAccessRow(t, "cveh_d2")
		if !ok {
			t.Fatal("an empty access_type must still produce a row — it is the fail-closed case")
		}
		if accessType != "" {
			t.Errorf("tesla_access_type = %q, want the empty string Tesla actually sent", accessType)
		}
	})

	// THE `"userId" = $2` PREDICATE IS A GUARD, NOT A CONVENIENCE. The upsert
	// runs a round trip after UpsertOwnedVehicle, so a car that changed hands in
	// between must not have a consent record filed against the wrong person.
	t.Run("writes nothing for a car owned by somebody else", func(t *testing.T) {
		seedVehicleForOwner(t, testPool, "cveh_d3", "5YJ3E1EA1NF000603", driverAccessOther)
		if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000603", driverAccessUser, "DRIVER", now); err != nil {
			t.Fatalf("RecordDriverAccess: %v", err)
		}
		if _, _, _, ok := readDriverAccessRow(t, "cveh_d3"); ok {
			t.Fatal("a driver-access row was filed against a user who does not hold the Vehicle row")
		}
	})

	t.Run("an unknown VIN is a silent no-op, not an error", func(t *testing.T) {
		if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000999", driverAccessUser, "DRIVER", now); err != nil {
			t.Fatalf("RecordDriverAccess on an unknown VIN = %v, want nil (best-effort by contract)", err)
		}
	})
}

// A RE-LINK REFRESHES WHAT TESLA SAYS AND NEVER RE-SHUTS THE GATE. Incidental
// re-links are common (the MYR-517 re-auth path runs one), and clobbering
// acknowledged_at would demand a second acknowledgment for a car that is
// already streaming.
func TestRecordDriverAccessDoesNotClobberAnAcknowledgment(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	prov := newTestProvisioner(t)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	linked := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	acked := time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)

	seedVehicleForOwner(t, testPool, "cveh_d4", "5YJ3E1EA1NF000604", driverAccessUser)
	if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000604", driverAccessUser, "DRIVER", linked); err != nil {
		t.Fatalf("RecordDriverAccess: %v", err)
	}
	if _, err := repo.AcknowledgeOwnerApproval(ctx, "cveh_d4", driverAccessUser, "owner-approval-v1", acked); err != nil {
		t.Fatalf("AcknowledgeOwnerApproval: %v", err)
	}

	// The re-link, with Tesla now spelling the access level differently.
	relinked := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000604", driverAccessUser, "DRIVER_2", relinked); err != nil {
		t.Fatalf("RecordDriverAccess (re-link): %v", err)
	}

	accessType, ackAt, version, ok := readDriverAccessRow(t, "cveh_d4")
	if !ok {
		t.Fatal("the row vanished on a re-link")
	}
	if accessType != "DRIVER_2" {
		t.Errorf("tesla_access_type = %q, want the re-link to refresh it to %q", accessType, "DRIVER_2")
	}
	if ackAt == nil || !ackAt.UTC().Equal(acked) {
		t.Fatalf("acknowledged_at = %v, want it untouched at %v — a re-link must not re-shut the gate", ackAt, acked)
	}
	if version == nil || *version != "owner-approval-v1" {
		t.Errorf("acknowledgment_version = %v, want it untouched", version)
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
	prov := newTestProvisioner(t)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	first := time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)
	second := time.Date(2026, 12, 25, 8, 0, 0, 0, time.UTC)

	seedVehicleForOwner(t, testPool, "cveh_d5", "5YJ3E1EA1NF000605", driverAccessUser)
	if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000605", driverAccessUser, "DRIVER", first.Add(-time.Hour)); err != nil {
		t.Fatalf("RecordDriverAccess: %v", err)
	}

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

	_, ackAt, version, ok := readDriverAccessRow(t, "cveh_d5")
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

// ClearDriverAccess is the access-UPGRADE case: Tesla now calls this account the
// car's OWNER, so the standing claim must go or the wire keeps calling a car
// "driver" that this person owns outright.
func TestClearDriverAccess(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	prov := newTestProvisioner(t)
	now := time.Date(2026, 9, 5, 10, 4, 0, 0, time.UTC)

	seedVehicleForOwner(t, testPool, "cveh_d7", "5YJ3E1EA1NF000607", driverAccessUser)
	seedVehicleForOwner(t, testPool, "cveh_d8", "5YJ3E1EA1NF000608", driverAccessOther)
	for _, v := range []struct{ vin, user string }{
		{"5YJ3E1EA1NF000607", driverAccessUser},
		{"5YJ3E1EA1NF000608", driverAccessOther},
	} {
		if err := prov.RecordDriverAccess(ctx, v.vin, v.user, "DRIVER", now); err != nil {
			t.Fatalf("seed driver access %s: %v", v.vin, err)
		}
	}

	if err := prov.ClearDriverAccess(ctx, "5YJ3E1EA1NF000607", driverAccessUser); err != nil {
		t.Fatalf("ClearDriverAccess: %v", err)
	}
	if _, _, _, ok := readDriverAccessRow(t, "cveh_d7"); ok {
		t.Error("the stale driver row survived an OWNER re-link")
	}
	// Owner-scoped, like the upsert: another person's row is untouched.
	if _, _, _, ok := readDriverAccessRow(t, "cveh_d8"); !ok {
		t.Error("another user's driver-access row was cleared")
	}

	// Idempotent — the overwhelmingly common case is an owner's car that never
	// had a row, and a missing row is success.
	if err := prov.ClearDriverAccess(ctx, "5YJ3E1EA1NF000607", driverAccessUser); err != nil {
		t.Errorf("second ClearDriverAccess = %v, want nil", err)
	}
}

// The VIN-keyed gate the fleet-config push route consults. Its three answers
// must be distinguishable, because two of them permit a push and one forbids it.
func TestPendingDriverAcknowledgmentByVIN(t *testing.T) {
	setupDriverAccess(t)
	ctx := context.Background()
	prov := newTestProvisioner(t)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	now := time.Date(2026, 9, 5, 10, 4, 0, 0, time.UTC)

	// An owner's car: no row.
	seedVehicleForOwner(t, testPool, "cveh_d9", "5YJ3E1EA1NF000609", driverAccessUser)
	// A driver's car, unacknowledged: the gate is SHUT.
	seedVehicleForOwner(t, testPool, "cveh_d10", "5YJ3E1EA1NF000610", driverAccessUser)
	if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000610", driverAccessUser, "DRIVER", now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A driver's car, acknowledged: the gate is OPEN.
	seedVehicleForOwner(t, testPool, "cveh_d11", "5YJ3E1EA1NF000611", driverAccessUser)
	if err := prov.RecordDriverAccess(ctx, "5YJ3E1EA1NF000611", driverAccessUser, "DRIVER", now); err != nil {
		t.Fatalf("seed: %v", err)
	}
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
