package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TripRepo.Delete against a real Postgres (MYR-607, §7.30.10).
//
// These assert what only a real database can answer: that FOUR tables empty in
// one transaction, that the two-link cascade to go_live_activities is actually
// reached, that the vehicle's DRIVES are untouched, that a second trip on the
// same car is untouched, and that the `trip.deleted` audit row lands inside the
// same transaction. Handler behaviour (which status each caller gets) is
// asserted in internal/telemetry with a hand-written store.

// tripChildCounts is the four-table census the deletion has to zero.
type tripChildCounts struct {
	participants int
	tokens       int
	legs         int
	activities   int
	trips        int
}

func countTripChildren(t *testing.T, tripID string) tripChildCounts {
	t.Helper()
	ctx := context.Background()
	var c tripChildCounts
	rows := []struct {
		query string
		into  *int
	}{
		{`SELECT count(*) FROM go_trip_participants WHERE trip_id = $1`, &c.participants},
		{`SELECT count(*) FROM go_trip_activity_tokens WHERE trip_id = $1`, &c.tokens},
		{`SELECT count(*) FROM go_trip_legs WHERE trip_id = $1`, &c.legs},
		{`SELECT count(*) FROM go_live_activities
		  WHERE trip_leg_id IN (SELECT id FROM go_trip_legs WHERE trip_id = $1)`, &c.activities},
		{`SELECT count(*) FROM go_trips WHERE id = $1`, &c.trips},
	}
	for _, r := range rows {
		if err := testPool.QueryRow(ctx, r.query, tripID).Scan(r.into); err != nil {
			t.Fatalf("count (%s): %v", r.query, err)
		}
	}
	return c
}

// seedFullTrip builds a trip with one of everything the deletion has to reach:
// a participant, a push-to-start token, a leg, and a Live Activity anchored to
// that leg.
//
// ⚠ THE ACTIVITY ROW IS THE POINT OF THIS FIXTURE. It hangs off the trip
// through TWO foreign keys (activity → leg → trip), so it is the row a
// hand-rolled deletion is most likely to miss — and the row it would leave
// behind is a live capability addressed at somebody's lock screen.
func seedFullTrip(t *testing.T, repo *store.TripRepo, vehicleID, shareID string, startsAt, endsAt time.Time) store.TripView {
	t.Helper()
	ctx := context.Background()

	trip := mustCreateTrip(t, repo, vehicleID, startsAt, endsAt, []string{shareID})

	if err := repo.RegisterActivityStartToken(ctx, trip.ID, shareViewer1, "pts-token-607", true); err != nil {
		t.Fatalf("RegisterActivityStartToken: %v", err)
	}

	legRepo := newTripLegRepo(t)
	leg, err := legRepo.StartLeg(ctx, trip.ID, vehicleID, "Grand Canyon Village", time.Now().UTC())
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}

	activities := newLiveActivityRepo(t)
	if err := activities.RegisterLegActivity(ctx, trip.ID, leg.ID, shareViewer1, "act-token-607", true); err != nil {
		t.Fatalf("RegisterLegActivity: %v", err)
	}
	return trip
}

// TestTripRepo_DeleteRemovesTheWholeAggregate is the headline: four tables
// empty, and nothing else moves.
func TestTripRepo_DeleteRemovesTheWholeAggregate(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := seedFullTrip(t, repo, vehicleID, shareID, now.Add(-time.Hour), now.Add(24*time.Hour))

	// A SECOND TRIP ON THE SAME CAR, in a window that does not overlap, so the
	// deletion's scoping is asserted rather than assumed: `WHERE trip_id = $1`
	// is one keystroke away from `WHERE vehicle_id = $1`.
	other := seedFullTrip(t, repo, vehicleID, shareID, now.Add(48*time.Hour), now.Add(72*time.Hour))

	// A DRIVE ON THE CAR, inside the deleted trip's window. A trip never owned
	// a drive — the window merely SELECTED it — so it must survive, and that is
	// the sentence the client's confirm dialog puts in front of the owner.
	driveRepo := store.NewDriveRepo(testPool, store.NoopMetrics{})
	if err := driveRepo.Create(ctx, store.DriveRecord{
		ID:          "drv_myr607_1",
		VehicleID:   vehicleID,
		Date:        now.Format("2006-01-02"),
		StartTime:   now.Format(time.RFC3339),
		RoutePoints: json.RawMessage("[]"),
	}); err != nil {
		t.Fatalf("Create drive: %v", err)
	}

	before := countTripChildren(t, trip.ID)
	if before.participants == 0 || before.tokens == 0 || before.legs == 0 || before.activities == 0 {
		t.Fatalf("fixture is incomplete: %+v — the test would pass vacuously", before)
	}

	if err := repo.Delete(ctx, trip.ID, shareOwnerA); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := countTripChildren(t, trip.ID); got != (tripChildCounts{}) {
		t.Errorf("after Delete the trip still has rows: %+v", got)
	}

	t.Run("the other trip on the same car is untouched", func(t *testing.T) {
		got := countTripChildren(t, other.ID)
		if got.trips != 1 || got.participants != 1 || got.tokens != 1 || got.legs != 1 || got.activities != 1 {
			t.Errorf("the neighbouring trip lost rows: %+v", got)
		}
	})

	t.Run("the drives survive", func(t *testing.T) {
		var count int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM "Drive" WHERE "id" = $1`, "drv_myr607_1").Scan(&count); err != nil {
			t.Fatalf("count drives: %v", err)
		}
		if count != 1 {
			t.Errorf("the drive went with the trip; a trip never owned one")
		}
	})

	t.Run("the trip is gone from every read", func(t *testing.T) {
		if _, err := repo.GetForUser(ctx, trip.ID, shareOwnerA); !errors.Is(err, store.ErrTripNotFound) {
			t.Errorf("GetForUser(owner) = %v, want ErrTripNotFound", err)
		}
		if _, err := repo.GetForUser(ctx, trip.ID, shareViewer1); !errors.Is(err, store.ErrTripNotFound) {
			t.Errorf("GetForUser(participant) = %v, want ErrTripNotFound", err)
		}
	})

	t.Run("a trip.deleted audit row was written", func(t *testing.T) {
		var (
			targetType string
			initiator  string
			userID     string
			metadata   []byte
		)
		if err := testPool.QueryRow(ctx,
			`SELECT "targetType", "initiator", "userId", "metadata"
			 FROM "AuditLog" WHERE action = 'trip.deleted' AND "targetId" = $1`,
			trip.ID).Scan(&targetType, &initiator, &userID, &metadata); err != nil {
			t.Fatalf("read audit row: %v", err)
		}
		if targetType != "trip" || initiator != "user" || userID != shareOwnerA {
			t.Errorf("audit row = {targetType:%q initiator:%q userId:%q}, want {trip user %s}",
				targetType, initiator, userID, shareOwnerA)
		}
		// CG-DL-5: metadata is P0 only. The trip NAME is P1 user content sealed
		// at rest, and an audit row is a place a value reaches permanent
		// storage without anybody deciding it should.
		var meta map[string]any
		if err := json.Unmarshal(metadata, &meta); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		if len(meta) != 1 || meta["vehicleId"] != vehicleID {
			t.Errorf("metadata = %v, want exactly {vehicleId: %s}", meta, vehicleID)
		}
		if string(metadata) != "" && containsSubstring(string(metadata), "DFW") {
			t.Errorf("the trip NAME reached the audit row: %s", metadata)
		}
	})
}

// TestTripRepo_DeleteRefusesEveryoneButTheOwner is the 404-not-403 rule at the
// STATEMENT level: the owner predicate is on the row that is written, so there
// is no check-then-write window a refactor could open.
func TestTripRepo_DeleteRefusesEveryoneButTheOwner(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := seedFullTrip(t, repo, vehicleID, shareID, now.Add(-time.Hour), now.Add(24*time.Hour))

	for _, caller := range []struct{ name, userID string }{
		{"a participant", shareViewer1},
		{"a stranger", "cusr_nobody_607"},
	} {
		t.Run(caller.name, func(t *testing.T) {
			if err := repo.Delete(ctx, trip.ID, caller.userID); !errors.Is(err, store.ErrTripNotFound) {
				t.Fatalf("Delete(%s) = %v, want ErrTripNotFound", caller.name, err)
			}
			if got := countTripChildren(t, trip.ID); got.trips != 1 || got.participants != 1 {
				t.Errorf("a refused delete removed rows: %+v", got)
			}
		})
	}

	t.Run("an unknown trip id", func(t *testing.T) {
		if err := repo.Delete(ctx, "ctrip_does_not_exist", shareOwnerA); !errors.Is(err, store.ErrTripNotFound) {
			t.Errorf("Delete(unknown) = %v, want ErrTripNotFound", err)
		}
	})
}

// TestTripRepo_DeleteIsIdempotentFromTheClientsSide: the second call finds no
// row and answers exactly as it would to a stranger.
//
// From the client's side that is indistinguishable from success and is meant to
// be — a delete that answered 404 on the retry of its own timed-out request
// would be a bug the app could not tell from a bug in the server.
func TestTripRepo_DeleteIsIdempotentFromTheClientsSide(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := seedFullTrip(t, repo, vehicleID, shareID, now.Add(-time.Hour), now.Add(24*time.Hour))

	if err := repo.Delete(ctx, trip.ID, shareOwnerA); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := repo.Delete(ctx, trip.ID, shareOwnerA); !errors.Is(err, store.ErrTripNotFound) {
		t.Errorf("second Delete = %v, want ErrTripNotFound", err)
	}
}

// TestTripRepo_DeleteWorksInEveryStatus pins the route's product rule at the
// store: scheduled, active and ENDED trips are all deletable.
//
// §7.30.4 refuses to MUTATE an ended trip — extending its window would
// resurrect live access every participant was already told had ended — and a
// deletion grants nothing to anybody, so the same refusal here would only
// strand finished trips on people's lists forever.
func TestTripRepo_DeleteWorksInEveryStatus(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	cases := []struct {
		name             string
		startsAt, endsAt time.Time
		endEarly         bool
	}{
		{"scheduled", now.Add(24 * time.Hour), now.Add(48 * time.Hour), false},
		{"active", now.Add(-time.Hour), now.Add(24 * time.Hour), false},
		{"ended", now.Add(-2 * time.Hour), now.Add(24 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One at a time: the overlap probe would refuse a second live
			// window on the same car.
			cleanTrips(t)
			trip := seedFullTrip(t, repo, vehicleID, shareID, tc.startsAt, tc.endsAt)
			if tc.endEarly {
				if _, err := repo.End(ctx, trip.ID, shareOwnerA); err != nil {
					t.Fatalf("End: %v", err)
				}
			}
			if err := repo.Delete(ctx, trip.ID, shareOwnerA); err != nil {
				t.Fatalf("Delete(%s): %v", tc.name, err)
			}
			if got := countTripChildren(t, trip.ID); got != (tripChildCounts{}) {
				t.Errorf("rows survived the delete of a %s trip: %+v", tc.name, got)
			}
		})
	}
}

// containsSubstring is a local helper: the store test package has no assertion
// library and this reads better than an inline strings.Contains at the one call
// site that needs it.
func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
