package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MYR-609 — the redeem path's PER-ROW accept, and the one constraint whose
// violation is a conflict rather than a fault. Split from
// vehicle_share_redeem.go under the 300-line rule.

// supersedeCollidingRows tombstones the locked pending rows that CANNOT become
// grants, and returns how many it retired.
//
// THE BUG IT FIXES, which §7.5.8 extend turned from a corner into a routine
// path (MYR-609). Accepting a code was all-or-nothing: one UPDATE over every
// row the code backs, so a single row colliding with an existing accepted grant
// raised 23505 and the WHOLE redemption failed with a 409. That was tolerable
// while the only way to hold a grant was to redeem one — the collision meant
// the person had already redeemed this very invite. It stopped being tolerable
// the moment an owner could ADD a car to somebody with one button: owner mints
// a two-car code for B and C, extends B onto that person before they get round
// to redeeming, and the code now bricks — C is grantable, B is not, and the
// redeemer is told 409 for both. The invite the owner sent becomes unusable by
// an action the owner took, with no way for either of them to see why.
//
// So the colliding rows are RETIRED rather than allowed to fail the batch: they
// are tombstoned `superseded` (revoked_by = 'owner', migration 0051), the rest
// accept normally, and the caller answers 409 only when EVERY row collided —
// which is the single-vehicle case, unchanged.
//
// TOMBSTONED, NOT LEFT PENDING. A row left behind would stay redeemable-looking
// until it expired, keep appearing on the owner's §7.5.2 listing as an
// outstanding invite for a car the person already has, and back a code its
// siblings had already consumed. `superseded` says which of those it was.
//
// AUTHORED BY THE OWNER, which matters because `revoked_by` is what §7.5.8
// consults before re-granting: this tombstone must not read as the grantee
// walking away from the car. Nobody walked away — the access is live, through
// the other row.
//
// The EXISTS is evaluated inside the UPDATE, over rows this transaction already
// holds under FOR UPDATE, so a grant appearing between a check and the write
// cannot slip through the gap.
func supersedeCollidingRows(ctx context.Context, tx pgx.Tx, ids []string, redeemerID string) (int, error) {
	tag, err := tx.Exec(ctx, querySupersedeCollidingShares, ids, redeemerID,
		ShareRevokedByOwner, ShareRevokedReasonSuperseded)
	if err != nil {
		return 0, fmt.Errorf("store.RedeemCode: supersede: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// constraintAcceptedGrant is the partial-unique index that enforces ONE
// accepted grant per (person, vehicle) — migration 0020. It is named here so
// the two paths that translate its violation into a 409 both branch on the
// index that actually decides, rather than on the SQLSTATE class.
const constraintAcceptedGrant = "uq_go_vehicle_shares_accepted_grant"

// isUniqueViolationOn reports whether err is a Postgres 23505 raised by ONE
// NAMED constraint.
//
// THE NAME IS THE POINT. A bare 23505 check answers "already shared" for any
// unique violation on the table — a primary-key collision on a freshly minted
// cuid, or whatever unique index the next migration adds — which reports a
// genuine server fault as a conflict the contract tells clients to render as
// success. Everything else falls through to the wrapped error and its 500.
func isUniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == constraint
}
