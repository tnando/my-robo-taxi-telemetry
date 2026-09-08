package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// RESUMING A LEG, against the real schema (MYR-612 review).
//
// The trips-package fake models the merge as a map lookup and cannot see the
// two things that actually decide whether a resumed leg raises a card: the
// per-DEVICE push-to-start claim on go_trip_activity_tokens, which lives in a
// different table, and the trip_id predicate on the probe. Both are properties
// of the statement, so both are asserted here or nowhere.

// resumeFixture seeds a trip window, a push-to-start registration for its
// owner, and returns the two repositories the resume path spans.
func resumeFixture(t *testing.T, tripID, vehicleID, ownerID string) (*store.TripLegRepo, *store.TripActivityTokenRepo) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, ownerID, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := testPool.Exec(ctx, `DELETE FROM go_trip_legs WHERE trip_id = $1`, tripID); err != nil {
		t.Fatalf("clean legs: %v", err)
	}
	if err := newTripRepo(t).RegisterActivityStartToken(ctx, tripID, ownerID, "pts_"+ownerID, false); err != nil {
		t.Fatalf("register push-to-start token: %v", err)
	}
	return newTripLegRepo(t), store.NewTripActivityTokenRepo(testPool, nil)
}

// TestResumeRecentLeg_GivesBackThePerDeviceClaim is the regression for a
// RESUMED LEG THAT RAISED NO CARD ANYWHERE.
//
// Every push-to-start is claimed twice — once for the leg
// (`activity_started_at`), once per device (`started_leg_id`) — so a token
// registered mid-leg can catch up without the fan-out double-sending. The
// resume released only the leg-level claim, and ClaimPushToStartForLeg's
// `IS DISTINCT FROM` then refused every device still stamped with that leg id:
// the journey came back with a banner, an open row, and nothing on any lock
// screen. That is worse than the duplicate leg the resume exists to prevent.
func TestResumeRecentLeg_GivesBackThePerDeviceClaim(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID, ownerID = "ctrip0612res", "cveh0612res", "cowner0612res"
	legs, tokens := resumeFixture(t, tripID, vehicleID, ownerID)

	now := time.Now().UTC()
	leg, err := legs.StartLeg(ctx, tripID, vehicleID, "Element by Marriott Sedona", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}
	if ok, err := legs.ClaimLegActivityStart(ctx, leg.ID); err != nil || !ok {
		t.Fatalf("ClaimLegActivityStart = %v, %v; want true", ok, err)
	}
	if _, claimed, err := tokens.ClaimPushToStartForLeg(ctx, tripID, ownerID, leg.ID); err != nil || !claimed {
		t.Fatalf("ClaimPushToStartForLeg = %v, %v; want true", claimed, err)
	}

	// The leg closes on a transient destination clear and its card is ended.
	if err := legs.EndLeg(ctx, leg.ID, now.Add(4*time.Minute), false); err != nil {
		t.Fatalf("EndLeg: %v", err)
	}
	if ok, err := legs.ClaimLegActivityEnd(ctx, leg.ID); err != nil || !ok {
		t.Fatalf("ClaimLegActivityEnd = %v, %v; want true", ok, err)
	}

	resumed, ok, err := legs.ResumeRecentLeg(ctx, tripID, vehicleID,
		"Element by Marriott Sedona", now)
	if err != nil {
		t.Fatalf("ResumeRecentLeg: %v", err)
	}
	if !ok {
		t.Fatal("the leg was not resumed; the same journey would open a second row")
	}
	if resumed.ID != leg.ID {
		t.Fatalf("resumed leg = %q, want %q", resumed.ID, leg.ID)
	}

	// BOTH claims must be back: the leg-level one, and the device's.
	if ok, err := legs.ClaimLegActivityStart(ctx, leg.ID); err != nil || !ok {
		t.Errorf("leg-level push-to-start claim = %v, %v; want true after a resume", ok, err)
	}
	tok, claimed, err := tokens.ClaimPushToStartForLeg(ctx, tripID, ownerID, leg.ID)
	if err != nil {
		t.Fatalf("ClaimPushToStartForLeg after resume: %v", err)
	}
	if !claimed {
		t.Fatal("the device's push-to-start claim was never released; the resumed leg " +
			"raises no card on any phone that held one")
	}
	if tok.PushToStartToken != "pts_"+ownerID {
		t.Errorf("claimed token = %q, want the registered one", tok.PushToStartToken)
	}
}

// TestResumeRecentLeg_KeepsTheClaimOfACardStillRunning is the other half of the
// same rule. A card that was never ENDED is still on the lock screen, and
// starting a second one for the same journey is exactly the duplicate the
// stamp exists to prevent — so a resume that did not undo an ending must undo
// no claim either.
func TestResumeRecentLeg_KeepsTheClaimOfACardStillRunning(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID, ownerID = "ctrip0612live", "cveh0612live", "cowner0612live"
	legs, tokens := resumeFixture(t, tripID, vehicleID, ownerID)

	now := time.Now().UTC()
	leg, err := legs.StartLeg(ctx, tripID, vehicleID, "Grand Canyon", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}
	if _, claimed, err := tokens.ClaimPushToStartForLeg(ctx, tripID, ownerID, leg.ID); err != nil || !claimed {
		t.Fatalf("ClaimPushToStartForLeg = %v, %v; want true", claimed, err)
	}
	// Closed WITHOUT the ending ever being delivered — the process died between
	// the row write and the APNs `end`.
	if err := legs.EndLeg(ctx, leg.ID, now.Add(2*time.Minute), false); err != nil {
		t.Fatalf("EndLeg: %v", err)
	}

	if _, ok, err := legs.ResumeRecentLeg(ctx, tripID, vehicleID, "Grand Canyon", now); err != nil || !ok {
		t.Fatalf("ResumeRecentLeg = %v, %v; want a resume", ok, err)
	}

	if _, claimed, err := tokens.ClaimPushToStartForLeg(ctx, tripID, ownerID, leg.ID); err != nil || claimed {
		t.Errorf("the device's claim was released while its card was still running "+
			"(claimed=%v, err=%v); that phone gets two cards for one journey", claimed, err)
	}
}

// TestResumeRecentLeg_NeverCrossesATrip is the regression for a leg ADOPTED BY
// THE WRONG TRIP.
//
// The merge window is a couple of minutes and a car very often begins its next
// trip inside one. Without the trip_id predicate the new trip resumed the old
// trip's leg: the row stayed attached to T1 while every delivery addressed T2's
// audience — a card for people no longer on the journey, no leg at all in T2's
// history, and no open leg T2's detector could ever close.
func TestResumeRecentLeg_NeverCrossesATrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const vehicleID, ownerID = "cveh0612xtrip", "cowner0612xtrip"
	const firstTrip, secondTrip = "ctrip0612xt1", "ctrip0612xt2"
	legs, _ := resumeFixture(t, firstTrip, vehicleID, ownerID)
	resumeFixture(t, secondTrip, vehicleID, ownerID)

	now := time.Now().UTC()
	leg, err := legs.StartLeg(ctx, firstTrip, vehicleID, "Sedona", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}
	if err := legs.EndLeg(ctx, leg.ID, now.Add(time.Minute), false); err != nil {
		t.Fatalf("EndLeg: %v", err)
	}

	// The next trip on the same car, to the same place, inside the window.
	if _, ok, err := legs.ResumeRecentLeg(ctx, secondTrip, vehicleID, "Sedona", now); err != nil {
		t.Fatalf("ResumeRecentLeg(second trip): %v", err)
	} else if ok {
		t.Error("a leg belonging to the previous trip was resumed into this one")
	}

	// The trip it actually belongs to still resumes it.
	resumed, ok, err := legs.ResumeRecentLeg(ctx, firstTrip, vehicleID, "Sedona", now)
	if err != nil {
		t.Fatalf("ResumeRecentLeg(own trip): %v", err)
	}
	if !ok || resumed.ID != leg.ID {
		t.Errorf("own-trip resume = (%q, %v), want (%q, true)", resumed.ID, ok, leg.ID)
	}
}
