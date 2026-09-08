package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-609 — the redeem path's PER-ROW accept.
//
// THE BUG THIS PINS is one §7.5.8 extend turned from a corner into a routine
// path. Accepting a code used to be all-or-nothing — one UPDATE over every row
// the code backs — so a single row colliding with an existing accepted grant
// raised 23505 and the WHOLE redemption failed with a 409. That was tolerable
// while the only way to hold a grant was to redeem one, because the collision
// meant the person had already redeemed this very invite. It stopped being
// tolerable the moment an owner could add a car with one button: mint a
// two-car code, extend one of those cars onto the person before they get round
// to redeeming, and the code bricks — the other car is grantable and they are
// told 409 for both, by an action the OWNER took, with neither of them able to
// see why.

// seedThirdOwnerACar adds a third car to owner A's fleet, which is what a
// partial collision needs: one car the redeemer already holds and one they do
// not.
func seedThirdOwnerACar(t *testing.T) string {
	t.Helper()
	const id = "cshveh0000000000000000a3"
	seedVehicleSummaryRow(t, id, shareOwnerA, vinForIndex(704), "Car A3", "Model X", 2025,
		"black", store.VehicleStatusParked, 70, 220)
	return id
}

func TestVehicleShareRepo_RedeemCode_SupersedesTheRowsThatCannotBecomeGrants(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	vehA3 := seedThirdOwnerACar(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	// The person already sees A1, the owner mints them a code for A2 + A3, and
	// then — before they redeem it — extends the A1 grant onto A2.
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
	invite := mustCreateInvite(t, repo, shareOwnerA, vehA2, []string{vehA2, vehA3},
		store.SharePermissionLive)
	if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
	}); err != nil {
		t.Fatalf("extend onto the car the pending code also covers: %v", err)
	}

	grants, err := repo.RedeemCode(ctx, invite.Code, shareViewer1)
	if err != nil {
		t.Fatalf("RedeemCode after an extend consumed one of its cars: %v — the invite the "+
			"owner sent must not be bricked by the owner's own later action", err)
	}
	if len(grants) != 1 || grants[0].VehicleID != vehA3 {
		t.Fatalf("granted %+v, want exactly the one car that was still grantable (%s)", grants, vehA3)
	}

	// And the access set is all three cars: two through the surviving rows,
	// one through the extend.
	ids := authAccessSet(t, shareViewer1)
	for _, want := range []string{vehA1, vehA2, vehA3} {
		if !slices.Contains(ids, want) {
			t.Errorf("access set %v omits %s", ids, want)
		}
	}

	// THE COLLIDING ROW IS TOMBSTONED, NOT LEFT PENDING. Left behind it would
	// stay redeemable-looking until it expired, keep appearing on the owner's
	// §7.5.2 listing as an outstanding invite for a car the person already has,
	// and back a code its siblings had already consumed.
	t.Run("the colliding row is retired as superseded, authored by the OWNER", func(t *testing.T) {
		var revokedBy, revokedReason *string
		if err := testPool.QueryRow(ctx,
			`SELECT revoked_by, revoked_reason FROM go_vehicle_shares
			 WHERE vehicle_id = $1 AND status = 'revoked'`, vehA2).
			Scan(&revokedBy, &revokedReason); err != nil {
			t.Fatalf("read the superseded row: %v", err)
		}
		if revokedBy == nil || *revokedBy != store.ShareRevokedByOwner {
			t.Errorf("revoked_by = %v, want %q — NOBODY WALKED AWAY: the access is live through "+
				"the other row, and a 'grantee' stamp here would make §7.5.8 refuse every later "+
				"extend onto this car", revokedBy, store.ShareRevokedByOwner)
		}
		if revokedReason == nil || *revokedReason != store.ShareRevokedReasonSuperseded {
			t.Errorf("revoked_reason = %v, want %q", revokedReason, store.ShareRevokedReasonSuperseded)
		}
	})

	// The consequence of that authorship, asserted end to end rather than
	// inferred from the column: a superseded tombstone must not block a later
	// extend onto the same car.
	t.Run("and a superseded tombstone does not block a later extend", func(t *testing.T) {
		if _, err := repo.LeaveVehicleShares(ctx, vehA2, shareViewer1); err != nil {
			t.Fatalf("leave A2 to clear the live grant: %v", err)
		}
		// The newest tombstone on A2 is now the GRANTEE's leave, so this is
		// refused — which is the point of the next step.
		if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
			OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
		}); !errors.Is(err, store.ErrShareGranteeLeft) {
			t.Fatalf("err = %v, want ErrShareGranteeLeft", err)
		}
		// Remove the leave and the SUPERSEDED tombstone is what is left. It is
		// owner-authored, so the extend goes through.
		if _, err := testPool.Exec(ctx,
			`DELETE FROM go_vehicle_shares
			 WHERE vehicle_id = $1 AND status = 'revoked' AND revoked_by = $2`,
			vehA2, store.ShareRevokedByGrantee); err != nil {
			t.Fatalf("remove the leave tombstone: %v", err)
		}
		if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
			OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
		}); err != nil {
			t.Fatalf("ExtendShare over a SUPERSEDED tombstone: %v — it records how the owner "+
				"composed an invite, not somebody walking away", err)
		}
	})
}

// The single-vehicle case is UNCHANGED: when every row in the code collides
// there is nothing to grant and nothing left to say but 409. What the per-row
// accept changed is that this is now reached only when it is the whole truth
// about the code rather than the fate of one row in it.
func TestVehicleShareRepo_RedeemCode_EveryRowCollidingIsStill409(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
	invite := mustCreateInvite(t, repo, shareOwnerA, vehA2, []string{vehA2}, store.SharePermissionLive)
	if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
	}); err != nil {
		t.Fatalf("extend onto the only car the code covers: %v", err)
	}

	if _, err := repo.RedeemCode(ctx, invite.Code, shareViewer1); !errors.Is(err, store.ErrShareAlreadyGranted) {
		t.Fatalf("err = %v, want ErrShareAlreadyGranted", err)
	}
	// AND NOTHING IS WRITTEN. The supersede ran inside the transaction, but the
	// 409 returns before the commit, so the rollback takes it with it — a
	// refused redemption leaves the row exactly as it found it. That is the
	// pre-MYR-609 behavior of this case, preserved: the retirement is a
	// consequence of a redemption that HAPPENED, and this one did not.
	if got := countQuery(t,
		`SELECT count(*) FROM go_vehicle_shares WHERE id = $1 AND status = 'pending'`,
		invite.ID); got != 1 {
		t.Errorf("the fully-colliding invite was mutated by a refused redemption; the 409 " +
			"returns before the commit, so the transaction must roll the supersede back")
	}
}
