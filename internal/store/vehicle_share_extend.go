package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
// The grantee keeps the same exits they had — §7.5.7 leave, and the row shows
// up in their catalog like every other accepted grant.

// AuditActionShareExtended records that an owner extended one of their
// ACCEPTED share grants onto another car they own (MYR-609).
//
// IT IS THE ONLY RECORD THAT THE ACCESS WAS NOT REDEEMED. Every other accepted
// row in go_vehicle_shares got there because somebody presented a code; this
// one got there because an owner pressed a button, and the row itself cannot
// say so — an extended grant is byte-for-byte an ordinary accepted grant, which
// is the point (every gate must treat it as one). So the audit row is where the
// distinction lives, and it is what answers "how did this person get access to
// this car when no invite for it was ever redeemed?".
//
// Metadata is TWO OPAQUE CUIDS AND NOTHING ELSE (CG-DL-5, P0-only): the new
// grant's id and the source grant's id. Deliberately no `label` and no `code` —
// both P1 (data-classification.md §1.15) — and deliberately not the GRANTEE's
// id either: the row is filed against the vehicle under the owner who acted,
// the two share ids resolve to the person for anybody with the database, and an
// audit row is a place a value reaches permanent storage without anybody
// deciding it should.
//
// DOTTED, like `trip.deleted` and `vehicle.owner_approval_acknowledged`: a
// share-scoped sub-action rather than a platform lifecycle verb.
const AuditActionShareExtended AuditAction = "share.extended"

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

// sourceGrant is the locked source row's copied half.
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
//  2. Verify the caller owns the TARGET vehicle, against the authoritative
//     relation. The handler checked it too; that check produces the good 403,
//     this one is what makes the write safe if a future caller skips it.
//  3. Probe for a live row the grantee already holds on the target →
//     ErrShareAlreadyGranted.
//  4. Mint a dead code (the schema requires one — see queryInsertExtendedShare).
//  5. Write the audit row BEFORE the grant (CG-DL-3), inside the same
//     transaction: a `share.extended` row that committed without the grant
//     would be a lie, and a grant that committed without the row would be the
//     access nobody could later explain.
//  6. INSERT the accepted row and return what the database wrote.
//
// Errors:
//   - ErrShareNotFound — the source is missing, is another owner's, is not
//     accepted, or names no grantee. All four INDISTINGUISHABLE, the same
//     non-oracle rule §7.5.3 and §7.5.7 hold for invite ids.
//   - ErrShareVehicleNotOwned — the target vehicle is not the caller's.
//   - ErrShareAlreadyGranted — the grantee already holds a live row on the
//     target vehicle. This is ALSO the answer when the source grant is already
//     on the target vehicle: extending a car onto itself is exactly the
//     already-shared case, told with the status that describes it rather than a
//     404 that would hide a row the caller demonstrably owns.
func (r *VehicleShareRepo) ExtendShare(ctx context.Context, in ExtendShareInput) (VehicleShare, error) {
	if err := validateExtendInput(in); err != nil {
		return VehicleShare{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(owner=%s): begin: %w", in.OwnerUserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	src, err := lockSourceGrant(ctx, tx, in)
	if err != nil {
		return VehicleShare{}, err
	}

	if err := verifyOwnsVehicle(ctx, tx, in.OwnerUserID, in.TargetVehicleID); err != nil {
		return VehicleShare{}, err
	}

	if err := refuseIfAlreadyShared(ctx, tx, in.TargetVehicleID, src.granteeID); err != nil {
		return VehicleShare{}, err
	}

	code, err := mintUnusedShareCode(ctx, tx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(owner=%s): %w", in.OwnerUserID, err)
	}

	id := newProvisionID()
	if err := insertShareExtendedAudit(ctx, tx, in, id); err != nil {
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(share=%s): %w", id, err)
	}

	row, err := insertExtendedShare(ctx, tx, id, in, src, code)
	if err != nil {
		return VehicleShare{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(share=%s): commit: %w", id, err)
	}
	return row, nil
}

// validateExtendInput rejects a malformed extend before it reaches the
// database, in the same spirit as validateCreateInput: the handler validates
// the same things to produce a good 400, and this is the repository refusing to
// write a row it cannot justify.
func validateExtendInput(in ExtendShareInput) error {
	switch {
	case strings.TrimSpace(in.OwnerUserID) == "":
		return errors.New("store.ExtendShare: empty owner id")
	case strings.TrimSpace(in.TargetVehicleID) == "":
		return errors.New("store.ExtendShare: empty target vehicle id")
	case strings.TrimSpace(in.SourceShareID) == "":
		return errors.New("store.ExtendShare: empty source share id")
	}
	return nil
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

// verifyOwnsVehicle fails unless the caller owns the target vehicle. READ-ONLY
// against the sibling-owned vehicle relation, reusing the create path's
// statement (CG-DL-9 permits reads).
//
// It is a SECOND check, not the only one: the handler already refused a
// non-owner with a 403 naming the vehicle. Both exist for the reason the top of
// vehicle_share_queries.go gives — the handler's check is the good error
// message, the one inside the transaction is what holds under concurrency and
// under a future caller who forgets.
func verifyOwnsVehicle(ctx context.Context, tx pgx.Tx, ownerID, vehicleID string) error {
	var owned string
	switch err := tx.QueryRow(ctx, queryShareOwnedVehicleIDs, []string{vehicleID}, ownerID).Scan(&owned); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrShareVehicleNotOwned
	case err != nil:
		return fmt.Errorf("store.ExtendShare(vehicle=%s): ownership check: %w", vehicleID, err)
	}
	return nil
}

// refuseIfAlreadyShared turns an existing live grant into ErrShareAlreadyGranted
// before any work is done. It is the readable half of an invariant the
// partial-unique index enforces regardless; see queryLiveShareForGrantee.
func refuseIfAlreadyShared(ctx context.Context, tx pgx.Tx, vehicleID, granteeID string) error {
	var one int
	switch err := tx.QueryRow(ctx, queryLiveShareForGrantee, vehicleID, granteeID).Scan(&one); {
	case err == nil:
		return ErrShareAlreadyGranted
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("store.ExtendShare(vehicle=%s): already-shared probe: %w", vehicleID, err)
	}
}

// insertExtendedShare writes the accepted row and returns it as the database
// holds it.
//
// A UNIQUE VIOLATION HERE IS THE CONFLICT, NOT A FAILURE. The probe above
// cannot serialise two concurrent extends of the same person onto the same car
// — both read no row, both proceed — so `uq_go_vehicle_shares_accepted_grant`
// is what decides, and the loser gets exactly the 409 the probe would have
// given it. Same mapping acceptLockedRows makes on the redeem path, for the
// same index.
func insertExtendedShare(
	ctx context.Context, tx pgx.Tx, id string, in ExtendShareInput, src sourceGrant, code string,
) (VehicleShare, error) {
	// NORMALIZE ON THE WAY IN, exactly as CreateInvite does. The source row may
	// predate MYR-369 and carry the retired live_history preset; copying it
	// verbatim would write a value the contract says is never written again,
	// into a row created today.
	permission := NormalizeSharePermission(src.permission)

	row, err := scanShare(tx.QueryRow(ctx, queryInsertExtendedShare,
		id, in.TargetVehicleID, in.OwnerUserID, src.label, permission, code,
		src.allowRides, src.suspendedAt, src.granteeID,
	))
	if err != nil {
		if isUniqueViolation(err) {
			return VehicleShare{}, ErrShareAlreadyGranted
		}
		return VehicleShare{}, fmt.Errorf("store.ExtendShare(share=%s): insert: %w", id, err)
	}
	return row, nil
}

// shareExtendedAuditMetadata is the `share.extended` row's metadata: two opaque
// cuids and nothing else (CG-DL-5, P0-only). See AuditActionShareExtended for
// what is deliberately absent and why.
type shareExtendedAuditMetadata struct {
	ShareID       string `json:"shareId"`
	SourceShareID string `json:"sourceShareId"`
}

// insertShareExtendedAudit writes the user-initiated `share.extended` AuditLog
// row inside the extend transaction, reusing the same-package queryAuditInsert
// column list (single source of truth shared with AuditRepo — keeps CG-DL-8
// column parity automatic).
//
// `targetType` is the VEHICLE and `targetId` the car gaining the grant, not the
// share row: the question this row is kept to answer is "who could see this car
// in June", which is asked about a car.
func insertShareExtendedAudit(ctx context.Context, tx pgx.Tx, in ExtendShareInput, shareID string) error {
	meta, err := json.Marshal(shareExtendedAuditMetadata{
		ShareID:       shareID,
		SourceShareID: in.SourceShareID,
	})
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(),                 // id (cuid)
		in.OwnerUserID,                   // userId (the owner who extended it)
		now,                              // timestamp
		string(AuditActionShareExtended), // action
		auditTargetTypeVehicle,           // targetType
		in.TargetVehicleID,               // targetId
		auditInitiatorUser,               // initiator
		meta,                             // metadata (two opaque cuids)
		now,                              // createdAt
	); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}
