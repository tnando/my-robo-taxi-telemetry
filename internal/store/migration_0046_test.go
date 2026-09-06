package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-599 — migration 0046's shape and its round trip.
//
// Four things about go_vehicle_driver_access are load-bearing enough that a
// silent change to any of them would be a CONSENT defect rather than a
// slowdown, which is a sharper class of bug than most schema drift:
//
//  1. THE PRIMARY KEY. One row per vehicle is what makes "is this car waiting
//     on an acknowledgment?" a single well-defined answer. A second row would
//     not be a second fact; it would be an ambiguity in a gate.
//  2. created_at IS NOT NULL. Row presence is read off it through the catalog's
//     LEFT JOIN (vehicle_driver_access.go says so at length), so a nullable
//     created_at would make an EXISTING driver row indistinguishable from an
//     absent one — i.e. would silently promote a driver car to owner access on
//     every read.
//  3. THE TWO ACKNOWLEDGMENT COLUMNS ARE NULLABLE. NULL is the shut gate. A
//     NOT NULL default would open every gate the moment a row was created.
//  4. THE PARTIAL INDEX over the unacknowledged rows, which is what the
//     reconciler's NOT EXISTS anti-join probes on every pass.
//
// There is deliberately NO foreign key to assert. MYR-599's issue text asks for
// `REFERENCES "Vehicle"("id") ON DELETE CASCADE`; CG-DL-9 forbids a file under
// internal/store/migrations/ from naming a Prisma-owned table at all, so the
// rows are swept explicitly by the teardown and by deletion step 8f instead.
// TestMigration0046_HasNoPrismaForeignKey pins that, because "the FK is absent"
// is a decision here rather than an oversight.

// TestMigration0046_DriverAccessShape pins the columns and their nullability.
func TestMigration0046_DriverAccessShape(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	cases := []struct {
		column      string
		wantNotNull bool
		because     string
	}{
		{"vehicle_id", true, "the primary key"},
		{"user_id", true, "step 8f reaches these rows by person"},
		{"tesla_access_type", true, "a row exists because a Tesla listing said something"},
		{"acknowledged_at", false, "NULL is the shut gate"},
		{"acknowledgment_version", false, "NULL exactly while acknowledged_at is NULL"},
		{
			"created_at", true,
			"row presence is read off it through the catalog LEFT JOIN — a NULL here " +
				"would make a driver car read as owner access",
		},
	}
	for _, tc := range cases {
		t.Run(tc.column, func(t *testing.T) {
			var isNullable string
			err := testPool.QueryRow(ctx,
				`SELECT is_nullable FROM information_schema.columns
				  WHERE table_name = 'go_vehicle_driver_access' AND column_name = $1`,
				tc.column).Scan(&isNullable)
			if err != nil {
				t.Fatalf("read column %s: %v (missing after migrate up?)", tc.column, err)
			}
			gotNotNull := isNullable == "NO"
			if gotNotNull != tc.wantNotNull {
				t.Errorf("%s is_nullable = %q, want NOT NULL = %v (%s)",
					tc.column, isNullable, tc.wantNotNull, tc.because)
			}
		})
	}

	var pk int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.table_constraints
		  WHERE table_name = 'go_vehicle_driver_access' AND constraint_type = 'PRIMARY KEY'`,
	).Scan(&pk); err != nil {
		t.Fatalf("read primary key: %v", err)
	}
	if pk != 1 {
		t.Fatal("go_vehicle_driver_access has no primary key; a car could carry two answers")
	}

	var idx int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		  WHERE tablename = 'go_vehicle_driver_access'
		    AND indexname = 'idx_go_vehicle_driver_access_pending'`,
	).Scan(&idx); err != nil {
		t.Fatalf("read pending index: %v", err)
	}
	if idx == 0 {
		t.Error("idx_go_vehicle_driver_access_pending missing; the reconciler's gate would scan")
	}
}

// TestMigration0046_OneRowPerVehicle proves the primary key actually refuses a
// second row — the property that makes the push gate a single answer.
func TestMigration0046_OneRowPerVehicle(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`DELETE FROM go_vehicle_driver_access WHERE vehicle_id = 'cveh0046pk'`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO go_vehicle_driver_access (vehicle_id, user_id, tesla_access_type)
		VALUES ('cveh0046pk', 'cuser0046a', 'DRIVER')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO go_vehicle_driver_access (vehicle_id, user_id, tesla_access_type)
		VALUES ('cveh0046pk', 'cuser0046b', 'DRIVER')`); err == nil {
		t.Fatal("a second driver-access row for one car was accepted; the gate is ambiguous")
	}

	// created_at defaults, so the catalog's presence read works on a row nobody
	// stamped explicitly — which is what the ON CONFLICT upsert relies on.
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_vehicle_driver_access
		  WHERE vehicle_id = 'cveh0046pk' AND created_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if n != 1 {
		t.Error("created_at did not default; the LEFT JOIN presence read would be blind")
	}

	if _, err := testPool.Exec(ctx,
		`DELETE FROM go_vehicle_driver_access WHERE vehicle_id = 'cveh0046pk'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestMigration0046_HasNoPrismaForeignKey pins the CG-DL-9 departure from the
// issue text as a DECISION rather than an omission: there is no FK to
// "Vehicle", which is why the teardown and deletion step 8f name these rows
// explicitly. A future FK added here would fail the contract-guard grep, but it
// would also silently make those two statements look redundant — so the absence
// is asserted where a reader will meet it.
func TestMigration0046_HasNoPrismaForeignKey(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	var fks int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.table_constraints
		  WHERE table_name = 'go_vehicle_driver_access' AND constraint_type = 'FOREIGN KEY'`,
	).Scan(&fks); err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}
	if fks != 0 {
		t.Errorf("go_vehicle_driver_access carries %d foreign key(s); CG-DL-9 forbids one to a "+
			"Prisma-owned table, and the teardown/step-8f deletes assume nothing cascades", fks)
	}
}

// TestMigration0046_DownDropsOnlyItsOwnTable RUNS THE REAL MIGRATION FILES,
// both of them, rather than asserting against a retyped copy — the
// TestMigration0025_DownDropsOnlyItsOwnTable pattern.
//
// It matters more here than for most tables because of what the down file
// destroys: this table is both the consent EVIDENCE and the push GATE, so a
// down that took a neighbour with it, or an up that failed to re-create the
// partial index, would leave the reconciler pushing configs at cars nobody
// acknowledged. golang-migrate is what will run these files in production, so
// that is the path worth exercising.
func TestMigration0046_DownDropsOnlyItsOwnTable(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if err := m.Migrate(45); err != nil {
		t.Fatalf("migrate down to 45: %v", err)
	}
	if tableExists(t, "go_vehicle_driver_access") {
		t.Error("go_vehicle_driver_access survived the down-migration")
	}
	// The rollback is surgical to the one table 0046 added: the keepalive table
	// 0045 created and the schedule table the gate reads alongside must remain.
	if !tableExists(t, "go_tesla_token_keepalive") {
		t.Fatal("the down-migration took go_tesla_token_keepalive with it")
	}
	if !tableExists(t, "go_fleet_config_attempts") {
		t.Fatal("the down-migration took go_fleet_config_attempts with it")
	}

	if err := m.Migrate(46); err != nil {
		t.Fatalf("migrate back up to 46: %v", err)
	}
	if !tableExists(t, "go_vehicle_driver_access") {
		t.Fatal("go_vehicle_driver_access missing after re-applying the up-migration")
	}

	// Re-assert the partial index after the round trip: this is what proves the
	// .up.sql file itself installs it, not merely that some earlier run did.
	var idx int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_indexes
		  WHERE tablename = 'go_vehicle_driver_access'
		    AND indexname = 'idx_go_vehicle_driver_access_pending'`,
	).Scan(&idx); err != nil {
		t.Fatalf("read pending index: %v", err)
	}
	if idx == 0 {
		t.Error("the pending index did not survive a down/up cycle")
	}
}
