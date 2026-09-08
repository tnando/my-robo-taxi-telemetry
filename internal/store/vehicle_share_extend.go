package store

import (
	"context"

	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MYR-609 — EXTEND an accepted grant onto a second car the same owner owns
// (rest-api.md §7.5.8, POST /api/vehicles/{vehicleId}/share/extend).
//
// THE HOLE IT FILLS. Before this, the only way to give somebody who already
// sees one of your cars a second one was to mint a fresh code and make them
// redeem it again — the tester report on the issue is an owner staring at
// "nobody is shared on this car" with the two people who ARE shared on his
// other car one screen away. Everything needed to serve that is already on the
// row: who holds the grant, what the owner called them, and what the grant
// conveys.
//
// THE CONSENT BASIS, because this is the one place in the sharing surface where
// access is granted WITHOUT the grantee performing an act. It is not a new
// relationship: this grantee already accepted a share FROM THIS OWNER, and the
// thing being added is another car belonging to that same owner. The owner
// could already do this unilaterally by any of three routes — a multi-vehicle
// invite at create time (§7.5.1 `vehicleIds`), a fresh code, or simply adding
// the car to an invite the person had not yet redeemed — so what the endpoint
// removes is the pointless second redemption, not the grantee's say. What it
// deliberately does NOT do is reach across owners: the source grant and the
// target car must both be the CALLER's, which is checked in SQL on both halves.
//
// THREE THINGS THE CONSENT BASIS DOES NOT COVER, each refused with its own 409:
//
//  1. A SUSPENDED SOURCE. A pause is the owner's own explicit state on a
//     relationship, and it is not a thing to propagate — copying it forward
//     writes a grant born paused that nobody would know to lift.
//  2. A SUSPENDED GRANT ALREADY ON THE TARGET. That person currently has
//     nothing on this car, so reporting it as `already_shared` — which the
//     contract tells clients to render as SUCCESS — would tell the owner a
//     paused person is fine.
//  3. A GRANT THE GRANTEE LEFT (§7.5.7). Leaving is the one exit a grantee
//     has. An endpoint that hands the access back on the owner's button press,
//     with no act by the grantee and no notification to them, is not an exit.
//
// The grantee keeps the same exits they had — §7.5.7 leave, and the row shows
// up in their catalog like every other accepted grant.

// ExtendShareInput is one owner extending one accepted grant onto one more of
// their own cars.
//
// THREE IDS AND NOTHING ELSE, which is the shape of the feature: the endpoint
// copies rather than composes, so there is no label, no permission and no flag
// to pass — offering one would let the caller create a grant that disagrees
// with the one it claims to extend.
type ExtendShareInput struct {
	// OwnerUserID is the caller. It must own BOTH the source grant and the
	// target vehicle; neither is taken on trust from the handler.
	OwnerUserID string
	// TargetVehicleID is the car gaining the grant — the path vehicle.
	TargetVehicleID string
	// SourceShareID is the accepted grant being extended.
	SourceShareID string
}

// sourceGrant is the locked source row's copied half, plus the two fields that
// are READ but never copied: `vehicleID` decides the extend-onto-itself case,
// and `suspendedAt` decides the paused-source refusal.
type sourceGrant struct {
	vehicleID   string
	label       string
	permission  string
	allowRides  bool
	suspendedAt *time.Time
	granteeID   string
}

// ExtendShare copies an accepted grant onto another vehicle the caller owns and
// returns the new row.
//
// ONE TRANSACTION, in this order, and the order is the argument:
//
//  1. LOCK the source grant (owner-scoped, accepted-only). Everything copied
//     comes from this row, so it is held for the length of the write.
//  2. Refuse a SUSPENDED source, and refuse a source already on the target
//     car. Both read only the row just locked, so neither costs a round trip.
//  3. Verify the caller owns the TARGET vehicle, against the authoritative
//     relation. The handler checked it too; that check produces the good 403,
//     this one is what makes the write safe if a future caller skips it.
//  4. Probe the TARGET for the three things that forbid the grant — a live row,
//     a paused row, a grantee-authored tombstone — in one statement.
//  5. Write the audit row BEFORE the grant (CG-DL-3), inside the same
//     transaction: a `share.extended` row that committed without the grant
//     would be a lie, and a grant that committed without the row would be the
//     access nobody could later explain.
//  6. INSERT the accepted row and return what the database wrote.
//
// THERE IS NO GO-SIDE INPUT VALIDATION, and its absence is deliberate. An
// earlier cut opened with a `validateExtendInput` returning bare `errors.New`
// values for empty ids, which the handler could only map to 500 — a malformed
// call answered as a server fault. Every one of those cases is already refused
// by a predicate that produces a MAPPED sentinel: an empty or unknown source id
// matches no row in step 1 (ErrShareNotFound → 404), and an empty or unknown
// target vehicle matches no row in step 3 (ErrShareVehicleNotOwned → 403). The
// SQL is the validation, and it is the only spelling of it that cannot disagree
// with itself.
//
// Errors:
//   - ErrShareNotFound — the source is missing, is another owner's, is not
//     accepted, or names no grantee. All four INDISTINGUISHABLE, the same
//     non-oracle rule §7.5.3 and §7.5.7 hold for invite ids.
//   - ErrShareVehicleNotOwned — the target vehicle is not the caller's.
//   - ErrShareSourceSuspended — the source grant is paused.
//   - ErrShareTargetSuspended — the grantee holds a PAUSED grant on the target.
//   - ErrShareGranteeLeft — the grantee left this car under §7.5.7.
//   - ErrShareAlreadyGranted — the grantee already holds a live, unpaused row
//     on the target vehicle. This is ALSO the answer when the source grant is
//     already on the target vehicle: extending a car onto itself is exactly the
//     already-shared case, told with the status that describes it rather than a
//     404 that would hide a row the caller demonstrably owns.
func (r *VehicleShareRepo) ExtendShare(ctx context.Context, in ExtendShareInput) (VehicleShare, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(owner=%s): begin: %w", in.OwnerUserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	src, err := lockSourceGrant(ctx, tx, in)
	if err != nil {
		return VehicleShare{}, err
	}
	if err := refuseUnextendableSource(src, in.TargetVehicleID); err != nil {
		return VehicleShare{}, err
	}

	if err := verifyOwnsAll(ctx, tx, "store.ExtendShare", in.OwnerUserID, []string{in.TargetVehicleID}); err != nil {
		return VehicleShare{}, err
	}

	if err := refuseBlockedTarget(ctx, tx, in.TargetVehicleID, src.granteeID); err != nil {
		return VehicleShare{}, err
	}

	id := newProvisionID()
	if err := insertShareExtendedAudit(ctx, tx, in, id); err != nil {
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(share=%s): %w", id, err)
	}

	row, err := insertExtendedShare(ctx, tx, id, in, src)
	if err != nil {
		return VehicleShare{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(share=%s): commit: %w", id, err)
	}
	return row, nil
}

// lockSourceGrant reads and locks the grant being extended. Any miss is
// ErrShareNotFound — see queryLockSourceAcceptedShare for the four predicates
// that collapse into it.
func lockSourceGrant(ctx context.Context, tx pgx.Tx, in ExtendShareInput) (sourceGrant, error) {
	var src sourceGrant
	err := tx.QueryRow(ctx, queryLockSourceAcceptedShare, in.SourceShareID, in.OwnerUserID).
		Scan(&src.vehicleID, &src.label, &src.permission, &src.allowRides, &src.suspendedAt, &src.granteeID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return sourceGrant{}, ErrShareNotFound
	case err != nil:
		// The source id, never the label it carries (P1).
		return sourceGrant{}, fmt.Errorf("store.ExtendShare(source=%s): lock: %w", in.SourceShareID, err)
	}
	return src, nil
}

// refuseUnextendableSource applies the two refusals that are decidable from the
// locked source row alone, before anything else is read.
//
// A SUSPENDED SOURCE is 409, not a copied pause. See
// queryLockSourceAcceptedShare for why it is selected and refused here rather
// than excluded by a predicate: the owner is looking at this row on their own
// listing, so the answer owes them the reason and the remedy.
//
// THE SOURCE ALREADY BEING ON THE TARGET is 409 `already_shared`, asserted
// EXPLICITLY here rather than left to the target probe two steps down. The
// probe would reach the same answer — the source row is itself a live row for
// this grantee on this car — but only by accident of the same query matching
// it, and the "different vehicle" rule is a rule the endpoint owes rather than
// an emergent property of one WHERE clause. Stated here it survives a rewrite
// of the probe, and it is the reading of `sourceGrant.vehicleID` that makes the
// field earn its scan.
func refuseUnextendableSource(src sourceGrant, targetVehicleID string) error {
	if src.suspendedAt != nil {
		return ErrShareSourceSuspended
	}
	if src.vehicleID == targetVehicleID {
		return ErrShareAlreadyGranted
	}
	return nil
}

// refuseBlockedTarget turns whatever already stands on the target car into the
// refusal it deserves, in ONE round trip. See queryExtendTargetBlock for the
// two columns and why each of the three answers is its own status rather than
// folded into `already_shared`.
//
// It is the READABLE half of an invariant the partial-unique index enforces
// regardless for the live-row case; the paused and left cases have no index
// behind them at all, and exist only here.
func refuseBlockedTarget(ctx context.Context, tx pgx.Tx, vehicleID, granteeID string) error {
	var liveBlock, tombstoneAuthor *string
	if err := tx.QueryRow(ctx, queryExtendTargetBlock, vehicleID, granteeID).
		Scan(&liveBlock, &tombstoneAuthor); err != nil {
		return fmt.Errorf("store.ExtendShare(vehicle=%s): target probe: %w", vehicleID, err)
	}

	if liveBlock != nil {
		switch *liveBlock {
		case extendBlockTargetSuspended:
			return ErrShareTargetSuspended
		default:
			return ErrShareAlreadyGranted
		}
	}
	if tombstoneAuthor != nil && *tombstoneAuthor == ShareRevokedByGrantee {
		return ErrShareGranteeLeft
	}
	return nil
}

// extendBlockTargetSuspended is the `live_block` projection's value for a
// PAUSED grant on the target. Its sibling, `already_shared`, is the default
// arm — a value the probe cannot produce is treated as the conservative
// refusal rather than as permission.
const extendBlockTargetSuspended = "target_suspended"

// insertExtendedShare writes the accepted row and returns it as the database
// holds it.
//
// A UNIQUE VIOLATION ON THE ACCEPTED-GRANT INDEX IS THE CONFLICT, NOT A
// FAILURE. The probe above cannot serialise two concurrent extends of the same
// person onto the same car — both read no row, both proceed — so
// `uq_go_vehicle_shares_accepted_grant` is what decides, and the loser gets
// exactly the 409 the probe would have given it. Same mapping acceptLockedRows
// makes on the redeem path, for the same index.
//
// THE CONSTRAINT NAME IS CHECKED, and that is the whole point of the narrowing.
// A bare "any 23505 is already-shared" would answer 409 for a primary-key
// collision on the freshly minted cuid, or for any unique index a later
// migration adds to this table — reporting a genuine server fault as a
// conflict the client is told to render as success. Anything other than the
// accepted-grant index falls through to the wrapped error and its 500.
func insertExtendedShare(
	ctx context.Context, tx pgx.Tx, id string, in ExtendShareInput, src sourceGrant,
) (VehicleShare, error) {
	// NORMALIZE ON THE WAY IN, exactly as CreateInvite does. The source row may
	// predate MYR-369 and carry the retired live_history preset; copying it
	// verbatim would write a value the contract says is never written again,
	// into a row created today.
	permission := NormalizeSharePermission(src.permission)

	row, err := scanShare(tx.QueryRow(ctx, queryInsertExtendedShare,
		id, in.TargetVehicleID, in.OwnerUserID, src.label, permission,
		src.allowRides, src.granteeID,
	))
	if err != nil {
		if isUniqueViolationOn(err, constraintAcceptedGrant) {
			return VehicleShare{}, ErrShareAlreadyGranted
		}
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(share=%s): insert: %w", id, err)
	}
	return row, nil
}
