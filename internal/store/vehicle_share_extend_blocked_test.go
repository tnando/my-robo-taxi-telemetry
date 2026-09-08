package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-609 — the two refusals that live ON THE TARGET CAR rather than on the
// source: a grant the owner has PAUSED there, and a car the grantee LEFT.
// Neither has an index behind it; the probe in queryExtendTargetBlock is the
// only thing that produces either, which is why they are exercised here against
// real rows rather than asserted from the sentinel's doc comment.

// extendOnto is the call under test, spelled once.
func extendOnto(t *testing.T, repo *store.VehicleShareRepo, target, sourceID string) (store.VehicleShare, error) {
	t.Helper()
	return repo.ExtendShare(context.Background(), store.ExtendShareInput{
		OwnerUserID: shareOwnerA, TargetVehicleID: target, SourceShareID: sourceID,
	})
}

// A PAUSED grant on the target is its own refusal and must NEVER borrow
// `already_shared`. A paused grant conveys nothing, so answering the sub-code
// the contract tells clients to render as SUCCESS would report "they already
// have this car" about somebody who currently has no access to it at all — and
// would leave the owner believing the pause they set is not the thing in their
// way when it is the only thing that is.
func TestVehicleShareRepo_ExtendShare_RefusesAPausedGrantOnTheTarget(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
	if _, err := extendOnto(t, repo, vehA2, sourceID); err != nil {
		t.Fatalf("seed the grant on the target: %v", err)
	}
	targetGrant := extendedGrantOn(t, repo, vehA2)
	if _, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
		InviteID: targetGrant.ID, OwnerUserID: shareOwnerA, Suspended: boolPtr(true),
	}); err != nil {
		t.Fatalf("pause the grant on the target: %v", err)
	}

	_, err := extendOnto(t, repo, vehA2, sourceID)
	if !errors.Is(err, store.ErrShareTargetSuspended) {
		t.Fatalf("err = %v, want ErrShareTargetSuspended", err)
	}
	if errors.Is(err, store.ErrShareAlreadyGranted) {
		t.Error("a paused grant on the target borrowed the already_shared sub-code, which tells " +
			"a client the call SUCCEEDED — for a person who currently has no access at all")
	}
	if got := countQuery(t,
		`SELECT count(*) FROM go_vehicle_shares WHERE vehicle_id = $1 AND status <> 'revoked'`,
		vehA2); got != 1 {
		t.Errorf("%d live rows on the target, want the 1 paused one — a second row was written "+
			"beside a grant that already exists", got)
	}

	t.Run("and lifting the pause makes it already_shared, which is the honest answer then", func(t *testing.T) {
		if _, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: targetGrant.ID, OwnerUserID: shareOwnerA, Suspended: boolPtr(false),
		}); err != nil {
			t.Fatalf("lift the pause: %v", err)
		}
		if _, err := extendOnto(t, repo, vehA2, sourceID); !errors.Is(err, store.ErrShareAlreadyGranted) {
			t.Fatalf("err = %v, want ErrShareAlreadyGranted", err)
		}
	})
}

// A car the grantee LEFT under §7.5.7 is refused, and the AUTHOR of the newest
// tombstone is what decides. Leaving is the one exit a grantee has; handing the
// access back on the owner's button press — no act by the grantee, no
// notification to them — would make that exit reversible by the party they were
// leaving.
func TestVehicleShareRepo_ExtendShare_RefusesACarTheGranteeLeft(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	// seedLeftCar puts a GRANTEE-authored tombstone on the target and returns
	// the source grant still live on the other car.
	seedLeftCar := func(t *testing.T) string {
		t.Helper()
		cleanVehicleShares(t)
		sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
		if _, err := extendOnto(t, repo, vehA2, sourceID); err != nil {
			t.Fatalf("seed the grant on the target: %v", err)
		}
		if got, err := repo.LeaveVehicleShares(ctx, vehA2, shareViewer1); err != nil || got != store.ShareLeaveDone {
			t.Fatalf("leave: result = %v, err = %v", got, err)
		}
		return sourceID
	}

	t.Run("the grantee left, so the owner cannot hand it back", func(t *testing.T) {
		sourceID := seedLeftCar(t)

		_, err := extendOnto(t, repo, vehA2, sourceID)
		if !errors.Is(err, store.ErrShareGranteeLeft) {
			t.Fatalf("err = %v, want ErrShareGranteeLeft", err)
		}
		if got := countQuery(t,
			`SELECT count(*) FROM go_vehicle_shares WHERE vehicle_id = $1 AND status = 'accepted'`,
			vehA2); got != 0 {
			t.Errorf("%d accepted rows on the car the grantee left, want 0", got)
		}
	})

	// The remedy the refusal names: a fresh invite, which the person can
	// decline by simply not redeeming it. That is the consent this endpoint is
	// otherwise entitled to assume, asked for again.
	t.Run("a fresh invite is the way back, and redeeming it works", func(t *testing.T) {
		seedLeftCar(t)

		invite := mustCreateInvite(t, repo, shareOwnerA, vehA2, []string{vehA2}, store.SharePermissionLive)
		if _, err := repo.RedeemCode(ctx, invite.Code, shareViewer1); err != nil {
			t.Fatalf("redeem the fresh invite: %v", err)
		}
		if ids := authAccessSet(t, shareViewer1); !slices.Contains(ids, vehA2) {
			t.Errorf("access set %v omits %s after a fresh redemption", ids, vehA2)
		}
	})

	// An OWNER tombstone is the ordinary case this endpoint exists for: an
	// owner re-sharing a car they themselves un-shared. It must not block.
	t.Run("an owner-authored tombstone does not block", func(t *testing.T) {
		cleanVehicleShares(t)
		sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
		if _, err := extendOnto(t, repo, vehA2, sourceID); err != nil {
			t.Fatalf("seed the grant on the target: %v", err)
		}
		if _, err := repo.RevokeInvite(ctx, extendedGrantOn(t, repo, vehA2).ID, shareOwnerA); err != nil {
			t.Fatalf("owner revoke: %v", err)
		}

		if _, err := extendOnto(t, repo, vehA2, sourceID); err != nil {
			t.Fatalf("ExtendShare over an OWNER tombstone: %v — re-sharing a car you un-shared "+
				"yourself is the ordinary case", err)
		}
	})

	// A tombstone predating migration 0051 has no recorded author and there is
	// no way to recover one. It fails OPEN, which is deliberate: blocking on
	// unknown would refuse every extend against a car carrying any historical
	// tombstone, for a leave that probably never happened.
	t.Run("a tombstone with no recorded author fails open", func(t *testing.T) {
		sourceID := seedLeftCar(t)
		if _, err := testPool.Exec(ctx,
			`UPDATE go_vehicle_shares SET revoked_by = NULL WHERE vehicle_id = $1 AND status = 'revoked'`,
			vehA2); err != nil {
			t.Fatalf("age the tombstone back to its pre-0051 shape: %v", err)
		}

		if _, err := extendOnto(t, repo, vehA2, sourceID); err != nil {
			t.Fatalf("ExtendShare over a pre-0051 tombstone: %v — unknown authorship must not "+
				"refuse every extend against a car with any history", err)
		}
	})

	// NEWEST WINS, and only the newest is consulted. A grantee who left, was
	// re-invited, accepted, and was then revoked BY THE OWNER has an owner
	// tombstone on top: they are extendable again. Reading the whole history
	// instead would make a single old leave permanent.
	t.Run("an owner tombstone written after the leave unblocks it", func(t *testing.T) {
		sourceID := seedLeftCar(t)

		invite := mustCreateInvite(t, repo, shareOwnerA, vehA2, []string{vehA2}, store.SharePermissionLive)
		if _, err := repo.RedeemCode(ctx, invite.Code, shareViewer1); err != nil {
			t.Fatalf("redeem the fresh invite: %v", err)
		}
		if _, err := repo.RevokeInvite(ctx, invite.ID, shareOwnerA); err != nil {
			t.Fatalf("owner revoke: %v", err)
		}

		if _, err := extendOnto(t, repo, vehA2, sourceID); err != nil {
			t.Fatalf("ExtendShare with an owner tombstone newer than the leave: %v", err)
		}
	})
}
