package store_test

import (
	"context"
	"testing"
	"time"
)

// The 0047 DOWN path, exercised for real (MYR-602).
//
// WHY A DOWN TEST AT ALL, when nothing in production runs one — the argument is
// TestMigration0048_DownThenUp's and is not repeated here. What IS particular to
// 0047 is that its rollback is the most consequential in this repo: it drops
// four tables and, in the same file, RESTORES a NOT NULL on a column the up-file
// relaxed. That last statement is the one that can fail against real data, and
// it fails for a reason no `IF EXISTS` spelling protects against — a surviving
// row with neither anchor. The down-file compensates with a DELETE ordered
// BEFORE it, and this test is what proves the DELETE is actually there and
// actually first.
//
// Run directly against the live schema rather than through golang-migrate's
// version counter, for the reason 0048's twin gives: the suite shares one
// container and one schema_migrations row.
//
// ⚠ THE LATER TRIP MIGRATIONS ARE ROLLED BACK FIRST, and rolled forward
// afterwards, which is what golang-migrate would do and what the pre-MYR-620
// version of this test only got away with by accident. 0049 and 0050 add an
// index and a column INSIDE the four tables, so dropping the tables took them
// with it and re-applying 0047 alone silently left the shared schema without
// them. 0051 made that latent hole visible: it adds a SEPARATE table with a
// foreign key onto go_trips, so `DROP TABLE go_trips` now fails outright
// (SQLSTATE 2BP01) rather than quietly under-restoring. The fix for both is the
// same — a rollback of 0047 is a rollback of everything built on top of it.
func TestMigration0047_DownThenUp(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	// The shared schema must come back whatever happens in between: every other
	// test in this package reads the four trip tables, the 0049 index, the 0050
	// column and the 0051 slot table.
	t.Cleanup(func() {
		for _, file := range tripMigrationUps {
			if _, err := testPool.Exec(ctx, readMigrationSQL(t, file)); err != nil {
				t.Fatalf("restoring %s failed; the shared schema is now incomplete: %v", file, err)
			}
		}
	})

	for _, table := range []string{"go_trips", "go_trip_participants", "go_trip_activity_tokens", "go_trip_legs"} {
		if !tableExists(t, table) {
			t.Fatalf("%s is missing before the test even runs", table)
		}
	}
	if !liveActivitiesHaveLegAnchor(t) {
		t.Fatal("go_live_activities has no trip_leg_id before the test even runs")
	}

	// PLANT THE ROW THE ROLLBACK MUST COPE WITH. A leg-anchored Live Activity
	// has ride_request_id NULL, so if the down-file's DELETE were missing or
	// ordered after the ALTER, the SET NOT NULL below would fail on this row —
	// which is the whole failure this test exists to catch, and which an empty
	// table would hide completely.
	now := time.Now().UTC()
	seedTripWindow(t, "ctrip0047down", "cveh0047down", "cowner0047down",
		now.Add(-time.Hour), now.Add(time.Hour))
	mustExec0047(t, `INSERT INTO go_trip_legs (id, trip_id, vehicle_id, destination_name_enc, started_at)
	                 VALUES ('cleg0047down', 'ctrip0047down', 'cveh0047down', 'enc', NOW())`)
	mustExec0047(t, `INSERT INTO go_live_activities (id, trip_leg_id, user_id, activity_push_token)
	                 VALUES ('cla0047down', 'cleg0047down', 'cuser0047down', 'tok-0047')`)

	// 0051 first: its table holds a foreign key onto go_trips, so it is part of
	// what "roll back the trips schema" means.
	if _, err := testPool.Exec(ctx, readMigrationSQL(t, "0051_trip_leg_banner_slots.down.sql")); err != nil {
		t.Fatalf("rolling back 0051 failed: %v", err)
	}
	if _, err := testPool.Exec(ctx, readMigrationSQL(t, "0047_trips.down.sql")); err != nil {
		t.Fatalf("down migration failed with a leg-anchored Activity present — "+
			"the rollback cannot cope with the data the up-file makes possible: %v", err)
	}
	if tableExists(t, "go_trip_leg_banners") {
		t.Error("go_trip_leg_banners survived its own rollback")
	}

	for _, table := range []string{"go_trips", "go_trip_participants", "go_trip_activity_tokens", "go_trip_legs"} {
		if tableExists(t, table) {
			t.Errorf("%s survived the down migration; the rollback is a no-op", table)
		}
	}
	if liveActivitiesHaveLegAnchor(t) {
		t.Error("go_live_activities kept its trip_leg_id column after the rollback")
	}

	// THE ANCHOR IS MANDATORY AGAIN, which is the invariant the up-file traded
	// away and the down-file has to buy back. Asserted by ATTEMPTING the write
	// it must refuse rather than by reading information_schema: a column can be
	// reported NOT NULL while a row that violates it still sits in the table,
	// and it is the refusal the ride path depends on.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, user_id, activity_push_token)
		 VALUES ('cla0047none', 'cuser0047down', 'tok-none')`); err == nil {
		t.Error("an anchorless Activity was accepted after the rollback; ride_request_id's NOT NULL did not come back")
		mustExec0047(t, `DELETE FROM go_live_activities WHERE id = 'cla0047none'`)
	}

	for _, file := range tripMigrationUps {
		if _, err := testPool.Exec(ctx, readMigrationSQL(t, file)); err != nil {
			t.Fatalf("re-applying %s after down failed; the pair is not a round trip: %v", file, err)
		}
	}
	for _, table := range []string{
		"go_trips", "go_trip_participants", "go_trip_activity_tokens", "go_trip_legs",
		"go_trip_leg_banners",
	} {
		if !tableExists(t, table) {
			t.Errorf("%s did not come back", table)
		}
	}
	if !liveActivitiesHaveLegAnchor(t) {
		t.Error("go_live_activities did not regain its trip_leg_id column")
	}
	// The 0050 column lives INSIDE a table 0047 drops, so re-applying 0047
	// alone would have left it behind — silently, until a push-to-start claim
	// failed at run time. This is the assertion that the roll-forward is whole.
	if !columnExists(t, "go_trip_activity_tokens", "started_leg_id") {
		t.Error("go_trip_activity_tokens.started_leg_id did not come back; the 0050 " +
			"roll-forward was skipped and every push-to-start claim would fail")
	}
}

// tripMigrationUps is the trips schema in dependency order: 0047 builds the
// four tables, 0049 indexes one of them, 0050 adds a column to another, and
// 0051 hangs a fifth table off go_trips. Rolling 0047 back means rolling back
// everything above it; rolling forward means replaying all of them.
//
// 0048 is absent deliberately — it touches go_push_prefs, not the trip tables,
// and has its own down test.
var tripMigrationUps = []string{
	"0047_trips.up.sql",
	"0049_trip_leg_resume.up.sql",
	"0050_trip_activity_token_leg_stamp.up.sql",
	"0051_trip_leg_banner_slots.up.sql",
}

// columnExists reports whether one column is currently installed.
func columnExists(t *testing.T, table, column string) bool {
	t.Helper()
	var present bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name = $1 AND column_name = $2)`, table, column).Scan(&present); err != nil {
		t.Fatalf("probe %s.%s: %v", table, column, err)
	}
	return present
}

// liveActivitiesHaveLegAnchor reports whether the second anchor column is
// currently installed.
func liveActivitiesHaveLegAnchor(t *testing.T) bool {
	t.Helper()
	var present bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name = 'go_live_activities' AND column_name = 'trip_leg_id')`).Scan(&present); err != nil {
		t.Fatalf("probe trip_leg_id: %v", err)
	}
	return present
}

// mustExec0047 runs one statement and fails the test on error. Local to this
// file so the rollback test carries no dependency on another test's helpers.
func mustExec0047(t *testing.T, sql string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
