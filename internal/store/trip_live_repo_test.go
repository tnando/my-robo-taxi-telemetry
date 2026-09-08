package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// The LIVE trip repositories against a real Postgres (MYR-602).
//
// What is asserted here is what only a real database can answer: that the
// CLAIMS are atomic, that the one-open-leg-per-trip partial index actually
// refuses a second leg, and that the two encrypted columns round-trip. Every
// one of those is a property of a statement or an index, not of Go code, so a
// fake would assert the test's own opinion rather than the schema's behaviour.

func seedTripWindow(t *testing.T, id, vehicleID, ownerID string, startsAt, endsAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_trips (id, vehicle_id, owner_user_id, name_enc, starts_at, ends_at)
		 VALUES ($1, $2, $3, 'seeded', $4, $5)
		 ON CONFLICT (id) DO UPDATE SET starts_at = $4, ends_at = $5,
		   started_notified_at = NULL, ended_notified_at = NULL, ended_at = NULL`,
		id, vehicleID, ownerID, startsAt, endsAt); err != nil {
		t.Fatalf("seed trip %s: %v", id, err)
	}
}

func newTripLiveRepo(t *testing.T) *store.TripLiveRepo {
	t.Helper()
	repo, err := store.NewTripLiveRepo(testPool, newTestEncryptor(t), nil, nil)
	if err != nil {
		t.Fatalf("NewTripLiveRepo: %v", err)
	}
	return repo
}

func newTripLegRepo(t *testing.T) *store.TripLegRepo {
	t.Helper()
	repo, err := store.NewTripLegRepo(testPool, newTestEncryptor(t), nil, nil)
	if err != nil {
		t.Fatalf("NewTripLegRepo: %v", err)
	}
	return repo
}

// TestTripClaims_AreOneShot is the property the whole sweeper rests on: each
// edge is claimed by exactly one caller, whatever races it.
func TestTripClaims_AreOneShot(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLiveRepo(t)

	now := time.Now().UTC()
	seedTripWindow(t, "ctrip0602open", "cveh0602", "cowner0602",
		now.Add(-time.Hour), now.Add(time.Hour))

	first, err := repo.ClaimTripsToStart(ctx, 50)
	if err != nil {
		t.Fatalf("ClaimTripsToStart: %v", err)
	}
	if !containsString(first, "ctrip0602open") {
		t.Fatalf("the open trip was not claimed: %v", first)
	}
	second, err := repo.ClaimTripsToStart(ctx, 50)
	if err != nil {
		t.Fatalf("ClaimTripsToStart (second): %v", err)
	}
	if containsString(second, "ctrip0602open") {
		t.Error("the same trip was claimed twice; the stamp is not arbitrating and every " +
			"participant would be told the trip started once a minute forever")
	}

	// The single-trip claim shares the stamp, which is what makes a
	// create-with-a-past-start racing the sweeper announce exactly once.
	claimed, err := repo.ClaimTripStartNow(ctx, "ctrip0602open")
	if err != nil {
		t.Fatalf("ClaimTripStartNow: %v", err)
	}
	if claimed {
		t.Error("ClaimTripStartNow won a stamp the sweeper had already taken")
	}
}

// TestClaimTripStartNow_RefusesAnUnopenedWindow. The end claim can be taken on
// the strength of a column the caller just wrote; a START has no such write
// behind it, so announcing one for a window that has not begun is the one
// direction the stamp can never take back.
func TestClaimTripStartNow_RefusesAnUnopenedWindow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLiveRepo(t)

	now := time.Now().UTC()
	seedTripWindow(t, "ctrip0602future", "cveh0602f", "cowner0602",
		now.Add(24*time.Hour), now.Add(48*time.Hour))

	claimed, err := repo.ClaimTripStartNow(ctx, "ctrip0602future")
	if err != nil {
		t.Fatalf("ClaimTripStartNow: %v", err)
	}
	if claimed {
		t.Error("a trip whose window has not opened was announced as started")
	}
}

// TestTripAudienceFor_ExcludesDepartedAndSuspended proves the two access
// predicates on the fan-out read. A notification naming somebody's car IS a
// surface, and a suspended grantee must be indistinguishable from no grantee on
// every one of them.
func TestTripAudienceFor_ExcludesDepartedAndSuspended(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLiveRepo(t)

	const tripID, vehicleID = "ctrip0602aud", "cveh0602aud"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602aud", now.Add(-time.Hour), now.Add(time.Hour))

	type participant struct {
		userID    string
		left      bool
		suspended bool
		revoked   bool
	}
	people := []participant{
		{userID: "cp0602live"},
		{userID: "cp0602left", left: true},
		{userID: "cp0602susp", suspended: true},
		{userID: "cp0602revk", revoked: true},
	}
	for _, p := range people {
		status := "accepted"
		if p.revoked {
			status = "revoked"
		}
		var suspendedAt any
		if p.suspended {
			suspendedAt = now
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_vehicle_shares (id, vehicle_id, owner_user_id, label, permission,
			   code, status, expires_at, accepted_by_user_id, suspended_at)
			 VALUES ('sh_' || $1, $2, 'cowner0602aud', 'Seeded', 'live',
			         'code_' || $1, $3, NOW() + INTERVAL '30 days', $1, $4)
			 ON CONFLICT (id) DO UPDATE SET status = $3, suspended_at = $4`,
			p.userID, vehicleID, status, suspendedAt); err != nil {
			t.Fatalf("seed share for %s: %v", p.userID, err)
		}
		var leftAt any
		if p.left {
			leftAt = now
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_trip_participants (trip_id, user_id, share_id, left_at)
			 VALUES ($1, $2, 'sh_' || $2, $3)
			 ON CONFLICT (trip_id, user_id) DO UPDATE SET left_at = $3`,
			tripID, p.userID, leftAt); err != nil {
			t.Fatalf("seed participant %s: %v", p.userID, err)
		}
	}

	got, err := repo.TripAudienceFor(ctx, tripID)
	if err != nil {
		t.Fatalf("TripAudienceFor: %v", err)
	}
	if got.VehicleID != vehicleID || got.OwnerUserID != "cowner0602aud" {
		t.Errorf("audience = %+v, want the seeded vehicle and owner", got)
	}
	if len(got.ParticipantUserIDs) != 1 || got.ParticipantUserIDs[0] != "cp0602live" {
		t.Errorf("participants = %v, want only cp0602live — departed, suspended and "+
			"revoked must every one be indistinguishable from absent",
			got.ParticipantUserIDs)
	}
}

// TestStartLeg_OneOpenLegPerTrip proves the partial unique index refuses a
// second open leg, and that the repo turns that refusal into the EXISTING leg
// rather than an error. One duplicate leg is one duplicate Live Activity on
// every participant's lock screen for the same journey.
func TestStartLeg_OneOpenLegPerTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID = "ctrip0602leg", "cveh0602leg"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602leg", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := testPool.Exec(ctx, `DELETE FROM go_trip_legs WHERE trip_id = $1`, tripID); err != nil {
		t.Fatalf("clean legs: %v", err)
	}

	repo := newTripLegRepo(t)
	first, err := repo.StartLeg(ctx, tripID, vehicleID, "Grand Canyon", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}
	if first.ID == "" {
		t.Fatal("StartLeg returned no leg")
	}
	// P1 ROUND TRIP: the destination is sealed on the way in and opened on the
	// way out, through the same MYR-447 label encryptor the vehicle repo uses.
	if first.DestinationName != "Grand Canyon" {
		t.Errorf("destination = %q, want Grand Canyon — the sealed column did not round-trip",
			first.DestinationName)
	}

	second, err := repo.StartLeg(ctx, tripID, vehicleID, "Sedona", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second StartLeg: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("a second leg %q was opened while %q was still open", second.ID, first.ID)
	}

	// Once it is closed, a new leg may open.
	if err := repo.EndLeg(ctx, first.ID, now.Add(time.Hour), true); err != nil {
		t.Fatalf("EndLeg: %v", err)
	}
	third, err := repo.StartLeg(ctx, tripID, vehicleID, "Sedona", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("third StartLeg: %v", err)
	}
	if third.ID == first.ID {
		t.Error("the closed leg was returned instead of a new one")
	}
}

// TestLegClaims_AreOneShot: four independent deliveries, four independent
// stamps. An alert can succeed while a push-to-start fails, and each must be
// retryable without re-sending the other.
func TestLegClaims_AreOneShot(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID = "ctrip0602claims", "cveh0602claims"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602claims", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := testPool.Exec(ctx, `DELETE FROM go_trip_legs WHERE trip_id = $1`, tripID); err != nil {
		t.Fatalf("clean legs: %v", err)
	}

	repo := newTripLegRepo(t)
	leg, err := repo.StartLeg(ctx, tripID, vehicleID, "Somewhere", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}

	claims := map[string]func(context.Context, string) (bool, error){
		"started_push":   repo.ClaimLegStartedPush,
		"arrived_push":   repo.ClaimLegArrivedPush,
		"activity_start": repo.ClaimLegActivityStart,
		"activity_end":   repo.ClaimLegActivityEnd,
	}
	for name, claim := range claims {
		t.Run(name, func(t *testing.T) {
			first, err := claim(ctx, leg.ID)
			if err != nil {
				t.Fatalf("first claim: %v", err)
			}
			if !first {
				t.Fatal("the first claim lost")
			}
			second, err := claim(ctx, leg.ID)
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if second {
				t.Error("the second claim also won; this delivery would repeat forever")
			}
		})
	}
}

// TestPushToStartTokens_UpsertAndReject covers the rotation (ActivityKit
// rotates the push-to-start token, so a re-registration must REPLACE rather
// than accumulate) and the 410 path that deletes by value.
func TestPushToStartTokens_UpsertAndReject(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID = "ctrip0602tok"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, "cveh0602tok", "cowner0602tok", now.Add(-time.Hour), now.Add(time.Hour))

	// The §7.30.8 writer is TripRepo's — one home for the upsert, per the
	// integration's "resolve duplicates toward the core lane" rule. This
	// repository owns only the SEND-side reads and the 410 delete.
	repo := store.NewTripActivityTokenRepo(testPool, nil)
	writer := newTripRepo(t)
	// The fan-out list re-joins membership and share, so the registrant must
	// be somebody the trip actually admits — here, the trip's own owner.
	if err := writer.RegisterActivityStartToken(ctx, tripID, "cowner0602tok", "pts-v1", true); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := writer.RegisterActivityStartToken(ctx, tripID, "cowner0602tok", "pts-v2", false); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	// The fan-out claim IS the read now (MYR-612 review): one statement that
	// stamps the leg on every admitted device and returns what it stamped.
	tokens, err := repo.ClaimPushToStartForLegAll(ctx, tripID, "cleg0602tok")
	if err != nil {
		t.Fatalf("claim fan-out: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("%d rows after a rotation, want 1 — keying on the token would accumulate "+
			"one row per rotation and leave the sender guessing which is live", len(tokens))
	}
	if tokens[0].PushToStartToken != "pts-v2" || tokens[0].Sandbox {
		t.Errorf("row = %+v, want the rotated token and its sandbox flag", tokens[0])
	}

	if err := repo.DeleteRejectedPushToStartToken(ctx, "pts-v2"); err != nil {
		t.Fatalf("delete rejected: %v", err)
	}
	// A NEW leg, so the claim is not refused by the previous stamp — what must
	// be gone is the ROW.
	tokens, err = repo.ClaimPushToStartForLegAll(ctx, tripID, "cleg0602tok2")
	if err != nil {
		t.Fatalf("claim after delete: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("%d rows survived the 410; the app is gone and the token addresses nothing",
			len(tokens))
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// TestRegisterLegActivity_UpsertsOnTheLegUserPair is the regression for the
// conflict target's missing INDEX PREDICATE.
//
// idx_go_live_activities_leg_user is PARTIAL (`WHERE trip_leg_id IS NOT NULL`),
// and Postgres refuses to infer a partial index unless the ON CONFLICT clause
// repeats its predicate. Without it EVERY call — not merely the second —
// failed with SQLSTATE 42P10 and every leg card was refused its update token,
// which is the whole Live Activity half of the feature addressing zero rows. So
// the first registration proves the arbiter is found at all, and the second
// proves it rotates in place rather than accumulating a row per rotation.
func TestRegisterLegActivity_UpsertsOnTheLegUserPair(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID = "ctrip0602reg", "cveh0602reg"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602reg", now.Add(-time.Hour), now.Add(time.Hour))

	legs := newTripLegRepo(t)
	leg, err := legs.StartLeg(ctx, tripID, vehicleID, "Grand Canyon Village", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}

	activities := newLiveActivityRepo(t)
	if err := activities.RegisterLegActivity(ctx, tripID, leg.ID, "cuser0602reg", "aaaa1111", true); err != nil {
		t.Fatalf("RegisterLegActivity: %v — a 42P10 here means the ON CONFLICT clause "+
			"lost the partial index's predicate and no leg card can ever register", err)
	}
	if err := activities.RegisterLegActivity(ctx, tripID, leg.ID, "cuser0602reg", "bbbb2222", false); err != nil {
		t.Fatalf("RegisterLegActivity (rotation): %v", err)
	}

	rows, err := activities.ActivitiesForLeg(ctx, leg.ID)
	if err != nil {
		t.Fatalf("ActivitiesForLeg: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows after a token rotation, want 1 — a second row is a second card "+
			"for one journey on one lock screen", len(rows))
	}
	if rows[0].ActivityPushToken != "bbbb2222" || rows[0].Sandbox {
		t.Errorf("row = %+v, want the rotated token and its sandbox flag", rows[0])
	}

	// The guard is the leg being open, in the leg's own vocabulary.
	if err := legs.EndLeg(ctx, leg.ID, now.Add(time.Minute), true); err != nil {
		t.Fatalf("EndLeg: %v", err)
	}
	err = activities.RegisterLegActivity(ctx, tripID, leg.ID, "cuser0602reg2", "cccc3333", false)
	if !errors.Is(err, store.ErrLiveActivityClosed) {
		t.Errorf("registering against a CLOSED leg = %v, want ErrLiveActivityClosed — "+
			"clearing that tombstone would resume an ETA countdown to a place the car left", err)
	}
}

// TestRegisterLegActivity_RefusesALegFromAnotherTrip is the §7.21.7 route's
// second guard, and it lives in the STATEMENT rather than in the handler.
//
// The handler establishes that the caller is on the TRIP. Nothing there
// establishes that the leg they named is that trip's, so an id from somebody
// else's journey would otherwise register a card on it — and the refusal has to
// be indistinguishable from "the leg has ended", or the endpoint becomes an
// oracle for leg ids.
func TestRegisterLegActivity_RefusesALegFromAnotherTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	now := time.Now().UTC()
	seedTripWindow(t, "ctrip0602mine", "cveh0602mine", "cowner0602x", now.Add(-time.Hour), now.Add(time.Hour))
	seedTripWindow(t, "ctrip0602theirs", "cveh0602theirs", "cowner0602y", now.Add(-time.Hour), now.Add(time.Hour))

	legs := newTripLegRepo(t)
	theirs, err := legs.StartLeg(ctx, "ctrip0602theirs", "cveh0602theirs", "Somewhere", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}

	activities := newLiveActivityRepo(t)
	err = activities.RegisterLegActivity(ctx, "ctrip0602mine", theirs.ID, "cuser0602x", "eeee5555", false)
	if !errors.Is(err, store.ErrLiveActivityClosed) {
		t.Errorf("registering against ANOTHER trip's open leg = %v, want ErrLiveActivityClosed "+
			"— a card would otherwise be raised on somebody else's journey", err)
	}
	rows, err := activities.ActivitiesForLeg(ctx, theirs.ID)
	if err != nil {
		t.Fatalf("ActivitiesForLeg: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d rows were written against another trip's leg", len(rows))
	}
}

// TestTripLegAccess_IsTheRouteGate covers §7.21.7's whole authorization, which
// the route can express no other way: `/api/trip-legs/{legId}/activity-token`
// carries no trip id, so who-may-register is resolved FROM the leg.
//
// The two refusals must stay distinguishable, and the test asserts both
// directions: a stranger (and an unknown leg) get ErrTripNotFound so the
// endpoint answers 404 identically for both and cannot be used to discover leg
// ids; a genuine MEMBER whose leg has ended gets a row with open=false, which
// the handler turns into the 409 that tells them to end the card locally.
func TestTripLegAccess_IsTheRouteGate(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID, ownerID = "ctrip0602acc", "cveh0602acc", "cowner0602acc"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, ownerID, now.Add(-time.Hour), now.Add(time.Hour))

	// A live participant, and one whose grant the owner suspended.
	for _, p := range []struct{ user, suspended string }{
		{"cp0602acclive", ""},
		{"cp0602accsusp", "now"},
	} {
		var suspendedAt any
		if p.suspended != "" {
			suspendedAt = now
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_vehicle_shares (id, vehicle_id, owner_user_id, label, permission,
			   code, status, expires_at, accepted_by_user_id, suspended_at)
			 VALUES ('sh_' || $1, $2, $3, 'Seeded', 'live', 'code_' || $1, 'accepted',
			         NOW() + INTERVAL '30 days', $1, $4)
			 ON CONFLICT (id) DO UPDATE SET suspended_at = $4`,
			p.user, vehicleID, ownerID, suspendedAt); err != nil {
			t.Fatalf("seed share for %s: %v", p.user, err)
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_trip_participants (trip_id, user_id, share_id)
			 VALUES ($1, $2, 'sh_' || $2) ON CONFLICT (trip_id, user_id) DO UPDATE SET left_at = NULL`,
			tripID, p.user); err != nil {
			t.Fatalf("seed participant %s: %v", p.user, err)
		}
	}

	legs := newTripLegRepo(t)
	leg, err := legs.StartLeg(ctx, tripID, vehicleID, "Grand Canyon Village", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}
	activities := newLiveActivityRepo(t)

	admitted := []struct {
		name   string
		userID string
	}{
		{"the owner, who holds no share on their own car", ownerID},
		{"a live participant", "cp0602acclive"},
	}
	for _, tt := range admitted {
		t.Run(tt.name, func(t *testing.T) {
			gotTrip, open, err := activities.TripLegAccess(ctx, leg.ID, tt.userID)
			if err != nil {
				t.Fatalf("TripLegAccess: %v", err)
			}
			if gotTrip != tripID || !open {
				t.Errorf("trip=%q open=%v, want %q/true", gotTrip, open, tripID)
			}
		})
	}

	refused := []struct {
		name   string
		legID  string
		userID string
	}{
		{"a stranger", leg.ID, "cstranger0602"},
		{"a SUSPENDED grantee, whose membership row is untouched", leg.ID, "cp0602accsusp"},
		{"an unknown leg", "cleg_does_not_exist", ownerID},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := activities.TripLegAccess(ctx, tt.legID, tt.userID)
			if !errors.Is(err, store.ErrTripNotFound) {
				t.Errorf("TripLegAccess = %v, want ErrTripNotFound — one answer for "+
					"'no such leg' and 'not yours', so the route cannot be used to "+
					"discover leg ids", err)
			}
		})
	}

	// A CLOSED leg for a genuine member is a DIFFERENT refusal: the row comes
	// back, with open=false, and the handler answers 409 rather than 404.
	if err := legs.EndLeg(ctx, leg.ID, now.Add(time.Minute), true); err != nil {
		t.Fatalf("EndLeg: %v", err)
	}
	gotTrip, open, err := activities.TripLegAccess(ctx, leg.ID, "cp0602acclive")
	if err != nil {
		t.Fatalf("TripLegAccess on a closed leg = %v, want a row with open=false — a member "+
			"holding a real card must be told to END it, not that it never existed", err)
	}
	if gotTrip != tripID || open {
		t.Errorf("trip=%q open=%v, want %q/false", gotTrip, open, tripID)
	}
}

// TestMarkLegActivitiesPushed_KeepsALiveCardOutOfTheReaper is finding 7.
//
// querySweepLiveActivities hard-DELETES any row whose updated_at is older than
// the cutoff, and nothing stamped the leg rows — the ride path's mark matches
// (ride_request_id, user_id) and never sees them. A card on a day-long drive
// would have had its row removed while it was still on the lock screen, taking
// the end push's only address with it.
func TestMarkLegActivitiesPushed_KeepsALiveCardOutOfTheReaper(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID = "ctrip0602mark", "cveh0602mark"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602mark", now.Add(-48*time.Hour), now.Add(time.Hour))

	legs := newTripLegRepo(t)
	leg, err := legs.StartLeg(ctx, tripID, vehicleID, "Grand Canyon Village", now.Add(-30*time.Hour))
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}
	activities := newLiveActivityRepo(t)
	if err := activities.RegisterLegActivity(ctx, tripID, leg.ID, "cuser0602mark", "dddd4444", false); err != nil {
		t.Fatalf("RegisterLegActivity: %v", err)
	}
	// Age the registration past the 24-hour horizon, as a real leg that has
	// been running for a day and a half would be.
	if _, err := testPool.Exec(ctx,
		`UPDATE go_live_activities SET updated_at = NOW() - INTERVAL '30 hours' WHERE trip_leg_id = $1`,
		leg.ID); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	moved, err := activities.MarkLegActivitiesPushed(ctx, leg.ID, []string{"cuser0602mark"})
	if err != nil {
		t.Fatalf("MarkLegActivitiesPushed: %v", err)
	}
	if moved != 1 {
		t.Fatalf("stamped %d rows, want 1", moved)
	}

	if _, err := activities.SweepStaleActivities(ctx, 24*time.Hour); err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	rows, err := activities.ActivitiesForLeg(ctx, leg.ID)
	if err != nil {
		t.Fatalf("ActivitiesForLeg: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the reaper removed a leg card that is still being pushed to; "+
			"its end push now has no address (rows=%d)", len(rows))
	}

	// A tombstoned row must NOT be un-staled by a racing send — the same rule
	// the ride twin holds, and the reason the statement is scoped to live rows.
	if _, err := activities.EndActivitiesForLeg(ctx, leg.ID); err != nil {
		t.Fatalf("EndActivitiesForLeg: %v", err)
	}
	moved, err = activities.MarkLegActivitiesPushed(ctx, leg.ID, []string{"cuser0602mark"})
	if err != nil {
		t.Fatalf("MarkLegActivitiesPushed (after end): %v", err)
	}
	if moved != 0 {
		t.Errorf("stamped %d ended rows, want 0", moved)
	}
}

// TestTripAudienceFor_SurvivesAnAllSuspendedRoster is finding 2.
//
// With the share predicate in the WHERE, a trip whose every participant's grant
// is suspended eliminated the trip ROW itself and the read answered
// ErrTripNotFound. The leg detector reads the audience on every frame of an
// open leg and returns on that error, so the leg never closed, the card was
// never ended, and the owner lost their banner — for a trip that plainly
// exists. The right answer is the trip with an EMPTY roster.
func TestTripAudienceFor_SurvivesAnAllSuspendedRoster(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLiveRepo(t)

	const tripID, vehicleID, ownerID = "ctrip0602susp", "cveh0602susp", "cowner0602susp"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, ownerID, now.Add(-time.Hour), now.Add(time.Hour))

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_vehicle_shares (id, vehicle_id, owner_user_id, label, permission,
		   code, status, expires_at, accepted_by_user_id, suspended_at)
		 VALUES ('sh_susponly', $1, $2, 'Seeded', 'live', 'code_susponly', 'accepted',
		         NOW() + INTERVAL '30 days', 'cp0602susponly', NOW())
		 ON CONFLICT (id) DO UPDATE SET suspended_at = NOW()`,
		vehicleID, ownerID); err != nil {
		t.Fatalf("seed suspended share: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_trip_participants (trip_id, user_id, share_id)
		 VALUES ($1, 'cp0602susponly', 'sh_susponly')
		 ON CONFLICT (trip_id, user_id) DO UPDATE SET left_at = NULL`,
		tripID); err != nil {
		t.Fatalf("seed participant: %v", err)
	}

	got, err := repo.TripAudienceFor(ctx, tripID)
	if err != nil {
		t.Fatalf("TripAudienceFor: %v — a trip whose whole roster is suspended must still "+
			"resolve, or its legs never close and its owner never hears anything", err)
	}
	if got.OwnerUserID != ownerID || got.VehicleID != vehicleID {
		t.Errorf("audience = %+v, want the seeded owner and vehicle", got)
	}
	if len(got.ParticipantUserIDs) != 0 {
		t.Errorf("participants = %v, want none — a suspended grantee is indistinguishable "+
			"from no grantee", got.ParticipantUserIDs)
	}
}

// TestClaimPushToStartForLegAll_DropsDepartedAndSuspended pins the fan-out's own
// access predicate. A push-to-start token is a standing CAPABILITY on a phone
// and nothing deletes it when a participant leaves or their share is suspended,
// so the list has to ask.
func TestClaimPushToStartForLegAll_DropsDepartedAndSuspended(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID, ownerID = "ctrip0602ptsacc", "cveh0602ptsacc", "cowner0602ptsacc"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, ownerID, now.Add(-time.Hour), now.Add(time.Hour))

	type person struct {
		userID    string
		left      bool
		suspended bool
	}
	people := []person{
		{userID: "cp0602ptslive"},
		{userID: "cp0602ptsleft", left: true},
		{userID: "cp0602ptssusp", suspended: true},
	}
	for _, p := range people {
		var suspendedAt, leftAt any
		if p.suspended {
			suspendedAt = now
		}
		if p.left {
			leftAt = now
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_vehicle_shares (id, vehicle_id, owner_user_id, label, permission,
			   code, status, expires_at, accepted_by_user_id, suspended_at)
			 VALUES ('sh_' || $1, $2, $3, 'Seeded', 'live', 'code_' || $1, 'accepted',
			         NOW() + INTERVAL '30 days', $1, $4)
			 ON CONFLICT (id) DO UPDATE SET suspended_at = $4`,
			p.userID, vehicleID, ownerID, suspendedAt); err != nil {
			t.Fatalf("seed share for %s: %v", p.userID, err)
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_trip_participants (trip_id, user_id, share_id, left_at)
			 VALUES ($1, $2, 'sh_' || $2, $3)
			 ON CONFLICT (trip_id, user_id) DO UPDATE SET left_at = $3`,
			tripID, p.userID, leftAt); err != nil {
			t.Fatalf("seed participant %s: %v", p.userID, err)
		}
	}

	writer := newTripRepo(t)
	for _, id := range []string{ownerID, "cp0602ptslive", "cp0602ptsleft", "cp0602ptssusp"} {
		if err := writer.RegisterActivityStartToken(ctx, tripID, id, "pts_"+id, false); err != nil {
			t.Fatalf("register token for %s: %v", id, err)
		}
	}

	repo := store.NewTripActivityTokenRepo(testPool, nil)
	rows, err := repo.ClaimPushToStartForLegAll(ctx, tripID, "cleg0602ptsfan")
	if err != nil {
		t.Fatalf("ClaimPushToStartForLegAll: %v", err)
	}
	got := make(map[string]bool, len(rows))
	for _, r := range rows {
		got[r.UserID] = true
	}
	if !got[ownerID] || !got["cp0602ptslive"] {
		t.Errorf("recipients = %v, want the owner and the live participant", got)
	}
	if got["cp0602ptsleft"] || got["cp0602ptssusp"] {
		t.Errorf("recipients = %v — a departed or suspended person still received a leg "+
			"card naming the car and its destination", got)
	}
}
