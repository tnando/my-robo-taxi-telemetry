package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The 0048 DOWN path, exercised for real.
//
// WHY A DOWN TEST AT ALL, when nothing in production runs one. Because a
// rollback is attempted exactly once, under pressure, by somebody who has
// already decided the deploy is going badly — and a `.down.sql` that has never
// been executed is a file, not a rollback. The `IF EXISTS` spelling in
// particular makes a broken down-file succeed silently.
//
// WHY IT RUNS THE SQL DIRECTLY rather than through golang-migrate's version
// counter: the suite shares one container and one schema_migrations row across
// every test in the package, so stepping the migrator down would leave whatever
// ran next looking at a half-migrated database. Executing the two files against
// the live schema and putting it back exercises exactly what an operator's
// `migrate down 1` would execute, and leaves the shared state where it was
// found.
func TestMigration0048_DownThenUp(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	// t.Cleanup rather than a trailing statement: a failure between the two
	// halves would otherwise leave the column missing and every later test in
	// the package failing for a reason that has nothing to do with them.
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, readMigrationSQL(t, "0048_trips_push_category.up.sql")); err != nil {
			t.Fatalf("restoring the trips column failed; the shared schema is now missing it: %v", err)
		}
	})

	if !pushPrefsHasTripsColumn(t) {
		t.Fatal("the trips column is missing before the test even runs")
	}

	if _, err := testPool.Exec(ctx, readMigrationSQL(t, "0048_trips_push_category.down.sql")); err != nil {
		t.Fatalf("down migration failed: %v", err)
	}
	if pushPrefsHasTripsColumn(t) {
		t.Error("the trips column survived the down migration; the rollback is a no-op")
	}

	if _, err := testPool.Exec(ctx, readMigrationSQL(t, "0048_trips_push_category.up.sql")); err != nil {
		t.Fatalf("re-applying up after down failed; the pair is not a round trip: %v", err)
	}
	if !pushPrefsHasTripsColumn(t) {
		t.Fatal("the trips column did not come back")
	}

	// AND IT COMES BACK ON, which is the part that matters to a person: a
	// rollback loses the stored preference, so re-applying restores the all-on
	// default rather than an off switch nobody chose. The down-file's header
	// says so out loud rather than leaving it to be discovered.
	var trips bool
	if err := testPool.QueryRow(ctx,
		`SELECT COALESCE(column_default = 'true', FALSE)
		   FROM information_schema.columns
		  WHERE table_name = 'go_push_prefs' AND column_name = 'trips'`).Scan(&trips); err != nil {
		t.Fatalf("read the restored default: %v", err)
	}
	if !trips {
		t.Error("the restored column does not default to TRUE")
	}
}

// readMigrationSQL loads one migration file from disk. Read from the source
// tree rather than through the package's embed.FS, because that variable is
// unexported and exporting it for a test would widen the package's API to make
// a rollback assertable.
func readMigrationSQL(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("migrations", name)
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative migration path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// pushPrefsHasTripsColumn reports whether go_push_prefs currently carries the
// trips column.
func pushPrefsHasTripsColumn(t *testing.T) bool {
	t.Helper()
	var present bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name = 'go_push_prefs' AND column_name = 'trips')`).Scan(&present); err != nil {
		t.Fatalf("probe the trips column: %v", err)
	}
	return present
}
