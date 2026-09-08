package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-608 — THE SERVER SAYS WHICH TRIP A DRIVE BELONGS TO, AND WHAT A TRIP
// ADDS UP TO.
//
// Until this issue the client mirrored the server's window rule to group drives
// under trips, and withheld a total whenever the page it held was smaller than
// `driveCount`. Two sources of truth for one fact, and a rider's collapsed
// header that could never state a mileage. These tests pin the two facts the
// server now states, against a real Postgres, because every one of them is a
// property of a SQL expression rather than of any Go function:
//
//	tripId  — window membership, resolved three different ways for three
//	          different callers, and never widened past what the caller may see
//	totals  — the window's drives summed in the statement that already counted
//	          them, running rather than final, and nullable rather than zero

// seedDriveWithStats installs one Drive row with a start instant AND the two
// figures the totals sum. `seedDriveAt` deliberately leaves both at their column
// defaults, so a totals test needs its own seeder rather than a widened one:
// the count tests next door assert on rows that sum to nothing, and giving them
// mileage would make two suites disagree about the same fixture.
func seedDriveWithStats(t *testing.T, driveID, vehicleID string, at time.Time, miles float64, minutes int) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Drive" ("id","vehicleId","date","startTime","distanceMiles","durationMinutes")
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		driveID, vehicleID, at.Format("2006-01-02"), at.UTC().Format(time.RFC3339), miles, minutes); err != nil {
		t.Fatalf("seed drive with stats: %v", err)
	}
}

// seedForeignTrip files a window owned by SOMEBODY ELSE directly, bypassing
// Create.
//
// Written as a raw INSERT on purpose. `Create` refuses an overlapping live
// window and validates the owner, and the state this fixture needs — a car that
// changed hands, so the PREVIOUS owner's trip still covers drives the CURRENT
// owner now reads — is reachable in production and not reachable through the
// constructor. The name is never read back, so any ciphertext-shaped value does.
func seedForeignTrip(t *testing.T, tripID, vehicleID, ownerUserID string, startsAt, endsAt time.Time) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_trips (id, vehicle_id, owner_user_id, name_enc, starts_at, ends_at)
		 VALUES ($1,$2,$3,'not-real-ciphertext',$4,$5)`,
		tripID, vehicleID, ownerUserID, startsAt, endsAt); err != nil {
		t.Fatalf("seed foreign trip: %v", err)
	}
}

// TestDriveTripIDIsBoundedByTheWindowAtBothEnds is the edge test, and it is the
// same set of instants TestTripDrivesAreBoundedByTheWindow uses.
//
// THE BOUND IS INCLUSIVE AT BOTH ENDS, matching `Trip.Window()`, §7.30.7 and the
// drive count. A drive that began exactly at the closing instant is a drive of
// that trip; one that began an instant later is not, and must carry a NULL
// rather than the nearest window's id.
func TestDriveTripIDIsBoundedByTheWindowAtBothEnds(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	drives := store.NewDriveRepo(testPool, store.NoopMetrics{})

	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	trip := mustCreateTrip(t, repo, vehicleID, start, end, []string{shareID})

	seeded := []struct {
		id     string
		at     time.Time
		inside bool
	}{
		{"cdrv_t_before", start.Add(-time.Second), false},
		{"cdrv_t_on_start", start, true},
		{"cdrv_t_inside", start.Add(24 * time.Hour), true},
		{"cdrv_t_on_end", end, true},
		{"cdrv_t_after", end.Add(time.Second), false},
	}
	for _, d := range seeded {
		seedDriveAt(t, d.id, vehicleID, d.at)
	}

	page, err := drives.ListByVehicleID(ctx, vehicleID, shareOwnerA, store.DriveListCursor{}, 20)
	if err != nil {
		t.Fatalf("ListByVehicleID: %v", err)
	}
	got := map[string]*string{}
	for _, item := range page.Items {
		got[item.ID] = item.TripID
	}

	for _, d := range seeded {
		id := got[d.id]
		switch {
		case d.inside && (id == nil || *id != trip.ID):
			t.Errorf("%s began inside the window but carries tripId %v, want %s", d.id, deref(id), trip.ID)
		case !d.inside && id != nil:
			t.Errorf("%s began outside the window but carries tripId %s", d.id, *id)
		}
	}

	// AND THE PARTICIPANT'S NARROWED LIST AGREES, drive for drive. Its window
	// set is the one that admitted the rows in the first place, so a row it
	// returns can never carry a NULL: being on the page is what makes the id
	// resolvable.
	t.Run("a participant's narrowed list stamps every row it returns", func(t *testing.T) {
		page, err := repo.VehicleDrivesInTripWindows(ctx, shareViewer1, vehicleID, store.DriveListCursor{}, 20)
		if err != nil {
			t.Fatalf("VehicleDrivesInTripWindows: %v", err)
		}
		if len(page.Items) != 3 {
			t.Fatalf("the participant was served %d drives, want the window's 3", len(page.Items))
		}
		for _, item := range page.Items {
			if item.TripID == nil || *item.TripID != trip.ID {
				t.Errorf("%s on a participant's page carries tripId %v, want %s", item.ID, deref(item.TripID), trip.ID)
			}
		}
	})

	// §7.30.7 STAMPS THE TRIP IN ITS OWN PATH. Every row it returns is inside
	// that window by the statement's own predicate, so the id can never be a
	// lie — and it cannot wander to an overlapping trip the way the §7.2
	// resolution deliberately can.
	t.Run("the trip's own drive list stamps the trip it was asked for", func(t *testing.T) {
		page, err := repo.TripDrivesForUser(ctx, trip.ID, shareViewer1, store.DriveListCursor{}, 20)
		if err != nil {
			t.Fatalf("TripDrivesForUser: %v", err)
		}
		if len(page.Items) != 3 {
			t.Fatalf("§7.30.7 returned %d drives, want 3", len(page.Items))
		}
		for _, item := range page.Items {
			if item.TripID == nil || *item.TripID != trip.ID {
				t.Errorf("%s carries tripId %v, want the path trip %s", item.ID, deref(item.TripID), trip.ID)
			}
		}
	})
}

// TestDriveTripIDPrefersTheNewestOverlappingWindow documents the tie-break, and
// documents why a tie is reachable at all.
//
// THE CREATE PROBE ONLY GUARDS WINDOWS THAT HAVE NOT CLOSED (`NOW() <
// COALESCE(ended_at, ends_at)`), because history does not reserve the calendar
// and a back-dated trip must be able to cover ground an old trip also covered.
// So two ENDED windows may overlap freely, and a drive can sit in both. One row
// carries one id, so the answer is the NEWEST window by `starts_at` — decided
// in the statement rather than left to the planner.
func TestDriveTripIDPrefersTheNewestOverlappingWindow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	drives := store.NewDriveRepo(testPool, store.NoopMetrics{})

	// Two windows over the same afternoon, both long since closed.
	older := mustCreateTrip(t, repo, vehicleID,
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC), []string{shareID})
	newer := mustCreateTrip(t, repo, vehicleID,
		time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC), []string{shareID})

	seedDriveAt(t, "cdrv_overlap", vehicleID, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	page, err := drives.ListByVehicleID(ctx, vehicleID, shareOwnerA, store.DriveListCursor{}, 10)
	if err != nil {
		t.Fatalf("ListByVehicleID: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("seeded 1 drive, listed %d", len(page.Items))
	}
	if id := page.Items[0].TripID; id == nil || *id != newer.ID {
		t.Fatalf("tripId = %v, want the NEWER overlapping window %s (older is %s)", deref(id), newer.ID, older.ID)
	}
}

// TestDriveTripIDIsScopedToTheCaller is the role test, and the hazard it pins is
// not hypothetical: a car changes hands, and the new owner reads a history that
// still contains the old owner's journeys.
//
// NAMING A TRIP THE CALLER CANNOT READ WOULD BE A DISCLOSURE — it says a
// stranger's trip exists, and hands over its id, on a surface that answers 404
// to anybody not on it. The expression is scoped to `owner_user_id = the
// caller`, so the answer is NULL rather than somebody else's window.
func TestDriveTripIDIsScopedToTheCaller(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	drives := store.NewDriveRepo(testPool, store.NoopMetrics{})

	mine := mustCreateTrip(t, repo, vehicleID,
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC), []string{shareID})
	seedForeignTrip(t, "ctrip_previous_owner", vehicleID, shareOwnerB,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC))

	seedDriveAt(t, "cdrv_mine", vehicleID, time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC))
	seedDriveAt(t, "cdrv_theirs", vehicleID, time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC))

	page, err := drives.ListByVehicleID(ctx, vehicleID, shareOwnerA, store.DriveListCursor{}, 10)
	if err != nil {
		t.Fatalf("ListByVehicleID: %v", err)
	}
	got := map[string]*string{}
	for _, item := range page.Items {
		got[item.ID] = item.TripID
	}
	if id := got["cdrv_mine"]; id == nil || *id != mine.ID {
		t.Errorf("the caller's own window was not named: %v", deref(id))
	}
	if id := got["cdrv_theirs"]; id != nil {
		t.Errorf("a drive inside ANOTHER account's window carried tripId %s — that id names a trip this caller cannot read", *id)
	}

	// A caller the layer cannot name resolves nothing, which is the same
	// answer and the safe one. §7.2's gate never reaches here without a
	// caller; this is the second line of defence.
	t.Run("an empty caller resolves no trip at all", func(t *testing.T) {
		page, err := drives.ListByVehicleID(ctx, vehicleID, "", store.DriveListCursor{}, 10)
		if err != nil {
			t.Fatalf("ListByVehicleID: %v", err)
		}
		for _, item := range page.Items {
			if item.TripID != nil {
				t.Errorf("%s carried tripId %s for a nameless caller", item.ID, *item.TripID)
			}
		}
	})
}

// TestDriveTripIDSurvivesAnUnreadableStartTime is the regression this issue
// found on its first test run.
//
// `Drive."startTime"` is a Prisma-owned TEXT column, and §7.2 carried no cast
// at all before MYR-608. An unguarded `::timestamptz` in the select list does
// not skip the row it cannot read — it ERRORS, and the owner's entire drive
// history becomes a permanent 500 because of one bad value. The guard turns
// that into a NULL `tripId` on that row and nothing else.
func TestDriveTripIDSurvivesAnUnreadableStartTime(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	drives := store.NewDriveRepo(testPool, store.NoopMetrics{})

	good := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	trip := mustCreateTrip(t, repo, vehicleID,
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), []string{shareID})
	seedDriveAt(t, "cdrv_readable", vehicleID, good)

	for _, junk := range []string{"", "10:00", "2026-07-02", "not a time"} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "Drive" ("id","vehicleId","date","startTime") VALUES ($1,$2,'2026-07-02',$3)`,
			"cdrv_junk_"+junk, vehicleID, junk); err != nil {
			t.Fatalf("seed junk drive %q: %v", junk, err)
		}
	}

	page, err := drives.ListByVehicleID(ctx, vehicleID, shareOwnerA, store.DriveListCursor{}, 20)
	if err != nil {
		t.Fatalf("one unreadable startTime took the whole list down: %v", err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("listed %d drives, want all 5 — the unreadable rows are still drives", len(page.Items))
	}
	for _, item := range page.Items {
		if item.ID == "cdrv_readable" {
			if item.TripID == nil || *item.TripID != trip.ID {
				t.Errorf("the readable row lost its tripId: %v", deref(item.TripID))
			}
			continue
		}
		if item.TripID != nil {
			t.Errorf("%s has an unreadable startTime but was placed in trip %s", item.ID, *item.TripID)
		}
	}
}

// TestTripTotalsSumTheWindow covers the totals for an ACTIVE window and an
// ENDED one, and the null that is not a zero.
//
// RUNNING TOTALS ARE THE POINT. An active trip reports what it has driven so
// far; withholding a total until the window closed would leave the surface that
// most wants one — a road trip in progress — the one surface that cannot have
// it. The client decides how to render a number that is still moving.
func TestTripTotalsSumTheWindow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-24*time.Hour), now.Add(24*time.Hour), []string{shareID})

	t.Run("an empty window totals NULL, not zero", func(t *testing.T) {
		view, err := repo.GetForUser(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("GetForUser: %v", err)
		}
		if view.DriveCount != 0 {
			t.Fatalf("driveCount = %d, want 0", view.DriveCount)
		}
		// SUM over zero rows is NULL, and it is carried through rather than
		// coalesced: `0` is a real total for a window whose drives went
		// nowhere, and a client that could not tell them apart would print
		// "0 mi" on a trip that has not begun.
		if view.TotalDistanceMiles != nil || view.TotalDurationMinutes != nil {
			t.Errorf("an empty window reported totals %v / %v, want both nil",
				view.TotalDistanceMiles, view.TotalDurationMinutes)
		}
	})

	seedDriveWithStats(t, "cdrv_tot_1", vehicleID, now.Add(-12*time.Hour), 12.5, 30)
	seedDriveWithStats(t, "cdrv_tot_2", vehicleID, now.Add(-6*time.Hour), 7.5, 15)
	// OUTSIDE the window on both sides, so the sums are the window's and not
	// the car's.
	seedDriveWithStats(t, "cdrv_tot_before", vehicleID, now.Add(-72*time.Hour), 100, 200)
	seedDriveWithStats(t, "cdrv_tot_after", vehicleID, now.Add(48*time.Hour), 100, 200)

	t.Run("an ACTIVE window reports what it has driven so far", func(t *testing.T) {
		view, err := repo.GetForUser(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("GetForUser: %v", err)
		}
		if view.StatusAt(time.Now()) != store.TripStatusActive {
			t.Fatalf("fixture is not active: %v", view.StatusAt(time.Now()))
		}
		assertTotals(t, view, 2, 20, 45)
	})

	t.Run("ending the window does not change what it already drove", func(t *testing.T) {
		if _, err := repo.End(ctx, trip.ID, shareOwnerA); err != nil {
			t.Fatalf("End: %v", err)
		}
		view, err := repo.GetForUser(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("GetForUser: %v", err)
		}
		if view.StatusAt(time.Now()) != store.TripStatusEnded {
			t.Fatalf("status = %v, want ended", view.StatusAt(time.Now()))
		}
		// The early end moved the effective end to NOW, which is after both
		// in-window drives and before the future one. The totals are unchanged
		// because the window still covers exactly the same two drives.
		assertTotals(t, view, 2, 20, 45)
	})

	t.Run("a participant reads the same totals the owner does", func(t *testing.T) {
		// A trip's numbers are a fact about the trip, not about who is asking.
		view, err := repo.GetForUser(ctx, trip.ID, shareViewer1)
		if err != nil {
			t.Fatalf("GetForUser(participant): %v", err)
		}
		assertTotals(t, view, 2, 20, 45)
	})
}

// assertTotals checks the three numbers that must describe ONE set of drives.
func assertTotals(t *testing.T, view store.TripView, wantCount int, wantMiles float64, wantMinutes int64) {
	t.Helper()
	if view.DriveCount != wantCount {
		t.Errorf("driveCount = %d, want %d", view.DriveCount, wantCount)
	}
	switch {
	case view.TotalDistanceMiles == nil:
		t.Errorf("totalDistanceMiles is nil, want %v", wantMiles)
	case *view.TotalDistanceMiles != wantMiles:
		t.Errorf("totalDistanceMiles = %v, want %v", *view.TotalDistanceMiles, wantMiles)
	}
	switch {
	case view.TotalDurationMinutes == nil:
		t.Errorf("totalDurationMinutes is nil, want %v", wantMinutes)
	case *view.TotalDurationMinutes != wantMinutes:
		t.Errorf("totalDurationMinutes = %v, want %v", *view.TotalDurationMinutes, wantMinutes)
	}
}

// TestTripListDecorationIssuesNoExtraQueryPerTrip is the N+1 guard, and it is
// the reason the totals ride the count's statement rather than getting one of
// their own.
//
// §7.30.2 DECORATES EVERY ROW IT RETURNS — vehicle, owner name, roster, drive
// totals, current leg — so its cost is `1 + 5N` statements and the constant is
// what a new field can quietly move. A separate `SELECT SUM(...)` would have
// made it `1 + 6N`: invisible on a list of one, a sixth of the round trips on a
// list of twenty, and nothing in the response would have shown it.
//
// COUNTED WITH A pgx QueryTracer over a private pool, because the repository's
// own metrics count REPOSITORY CALLS and this is a claim about STATEMENTS. A
// counter that could not tell "one call that issues six queries" from "one call
// that issues five" would pass either way.
func TestTripListDecorationIssuesNoExtraQueryPerTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)

	tracer := &countingTracer{}
	pool := newTracedPool(t, tracer)
	repo := store.NewTripRepo(pool, store.NoopMetrics{}, newTestEncryptor(t), testLogger())

	// Trips are created through the traced pool too; the counter is reset
	// before each measured read, so the writes are not in either sample.
	now := time.Now().UTC()
	base := now.Add(-240 * time.Hour)
	for i := 0; i < 3; i++ {
		// Non-overlapping historical windows — the create probe refuses
		// overlapping LIVE ones, and three ended trips make the per-row
		// decoration comparable across the two samples.
		from := base.Add(time.Duration(i) * 48 * time.Hour)
		if _, err := repo.Create(ctx, store.CreateTripInput{
			VehicleID:           vehicleID,
			OwnerUserID:         shareOwnerA,
			Name:                "leg",
			StartsAt:            from,
			EndsAt:              from.Add(24 * time.Hour),
			ParticipantShareIDs: []string{shareID},
		}); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}
	seedDriveWithStats(t, "cdrv_n1", vehicleID, base.Add(time.Hour), 5, 10)

	measure := func(limit int) int {
		tracer.reset()
		views, err := repo.ListForUser(ctx, shareOwnerA, "", limit)
		if err != nil {
			t.Fatalf("ListForUser: %v", err)
		}
		if len(views) != limit {
			t.Fatalf("ListForUser(limit=%d) returned %d trips", limit, len(views))
		}
		return tracer.count()
	}

	one := measure(1)
	three := measure(3)

	// FOUR DECORATION STATEMENTS PER ENDED TRIP: vehicle, owner first name,
	// roster, drive totals. The fifth — the open leg — is skipped without
	// asking the database when the window is not active, which is why the
	// fixture uses ENDED windows: the count is then a constant rather than a
	// function of what the leg detector happened to leave behind.
	//
	// MYR-608 added a THIRD NUMBER to the fourth statement and no fifth
	// statement. A `SELECT SUM(...)` of its own would make this 5.
	const wantPerTrip = 4
	if got := (three - one) / 2; got != wantPerTrip {
		t.Fatalf("each additional trip costs %d statements, want %d (1 trip = %d, 3 trips = %d) — "+
			"a totals query of its own would show up here", got, wantPerTrip, one, three)
	}
	if want := 1 + wantPerTrip; one != want {
		t.Fatalf("a one-trip list issued %d statements, want %d (the list + its decoration)", one, want)
	}
}

// countingTracer counts every statement pgx sends on the pool it is installed
// on. Deliberately not a metrics fake: the claim under test is about
// STATEMENTS, and the repository's metrics count repository CALLS.
type countingTracer struct {
	n int
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n++
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *countingTracer) reset() { c.n = 0 }
func (c *countingTracer) count() int {
	return c.n
}

// newTracedPool builds a SINGLE-CONNECTION pool with the tracer installed.
//
// One connection, because the tracer is not synchronised and a pool that opened
// a second connection under load would count from two goroutines. The reads
// under test are sequential by construction, so one is enough and makes the
// count deterministic.
func newTracedPool(t *testing.T, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testConnStr)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// deref renders a nullable id for a failure message without a nil check at
// every call site.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
