package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The rider side of MYR-184 vehicle sharing: redeeming a code, and the two
// lookups the authorization layer needs afterwards (the viewer access set and
// the per-vehicle tier).
//
// The code being redeemed is P1 and is NEVER logged and NEVER echoed into an
// error string, including on failure — an error message naming a rejected code
// would hand an enumerating caller a confirmation oracle.

// RedeemCode accepts EVERY pending, unexpired row backing a code, atomically,
// on behalf of redeemerID.
//
// The whole redemption is one transaction with the candidate rows held under
// FOR UPDATE, which is what makes the partial states unreachable:
//
//   - Two people redeeming the same code concurrently: the second blocks on the
//     lock, re-reads no pending rows, and gets ErrShareNotFound (404). It never
//     sees a subset granted.
//   - The caller owning one of the target vehicles: nothing is written at all —
//     the transaction rolls back before the UPDATE, so a multi-vehicle code
//     that includes one of your own cars grants you none of the others rather
//     than a confusing partial set.
//   - A retried request from the SAME person after a dropped response: no
//     pending rows remain, so the accepted-by-me lookup answers instead and the
//     same grants come back with a 200.
//
// Invalid, expired, and consumed-by-someone-else codes all produce
// ErrShareNotFound so the caller cannot tell them apart.
func (r *VehicleShareRepo) RedeemCode(ctx context.Context, code, redeemerID string) ([]ShareGrant, error) {
	if strings.TrimSpace(redeemerID) == "" {
		return nil, errors.New("store.RedeemCode: empty redeemer id")
	}
	if !ValidShareCodeFormat(code) {
		// Never report the value — only that it did not match the shape.
		return nil, ErrShareNotFound
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.RedeemCode: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids, owner, err := lockPendingRows(ctx, tx, code)
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		// Nothing redeemable. Either this person already redeemed it (200,
		// idempotent) or the code is dead in one of the three
		// indistinguishable ways (404).
		return r.alreadyRedeemed(ctx, tx, code, redeemerID)
	}
	if owner == redeemerID {
		return nil, ErrShareSelfRedeem
	}

	// PER-ROW ACCEPT (MYR-609). Retire the rows that CANNOT become grants
	// before trying to accept any of them — see supersedeCollidingRows.
	superseded, err := supersedeCollidingRows(ctx, tx, ids, redeemerID)
	if err != nil {
		return nil, err
	}

	grants, err := acceptLockedRows(ctx, tx, ids, redeemerID)
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		if superseded > 0 {
			// EVERY row in the code collided with a grant this person
			// already holds. Nothing was granted and nothing is left to
			// grant, which is the already-shared conflict — the same 409
			// the single-vehicle case has always produced, now reached
			// only when it is the whole truth about the code rather than
			// the fate of one row in it.
			return nil, ErrShareAlreadyGranted
		}
		// The rows were locked and pending a statement ago; zero updated
		// means the expiry boundary was crossed mid-transaction.
		return nil, ErrShareNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store.RedeemCode: commit: %w", err)
	}
	return grants, nil
}

// lockPendingRows selects and locks the redeemable rows for a code, returning
// their ids and the single owner they all belong to.
//
// A code resolving to more than one owner can only happen through an
// astronomically unlikely mint collision, and granting it would hand the
// redeemer access to two unrelated people's cars. It is refused, not guessed.
func lockPendingRows(ctx context.Context, tx pgx.Tx, code string) (ids []string, owner string, err error) {
	rows, err := tx.Query(ctx, queryLockPendingByCode, code)
	if err != nil {
		return nil, "", fmt.Errorf("store.RedeemCode: lock candidates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, vehicleID, ownerID, permission string
		if err := rows.Scan(&id, &vehicleID, &ownerID, &permission); err != nil {
			return nil, "", fmt.Errorf("store.RedeemCode: scan candidate: %w", err)
		}
		if owner != "" && ownerID != owner {
			return nil, "", ErrShareCodeCollision
		}
		owner = ownerID
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store.RedeemCode: iterate candidates: %w", err)
	}
	return ids, owner, nil
}

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

// acceptLockedRows flips the locked ids to accepted and returns what was
// granted. A unique violation here is the partial-unique accepted-grant index
// refusing a SECOND grant of the same vehicle to the same person through a
// different invite — a conflict, not a failure.
//
// supersedeCollidingRows has already retired every row this transaction could
// see colliding, so reaching this mapping means a CONCURRENT redemption or
// extend won the index between the two statements. The mapping stays because
// that race is real; it is now the only way to get here.
func acceptLockedRows(ctx context.Context, tx pgx.Tx, ids []string, redeemerID string) ([]ShareGrant, error) {
	rows, err := tx.Query(ctx, queryAcceptSharesByID, ids, redeemerID)
	if err != nil {
		if isUniqueViolationOn(err, constraintAcceptedGrant) {
			return nil, ErrShareAlreadyGranted
		}
		return nil, fmt.Errorf("store.RedeemCode: accept: %w", err)
	}
	defer rows.Close()

	grants, err := scanGrants(rows)
	if err != nil {
		if isUniqueViolationOn(err, constraintAcceptedGrant) {
			return nil, ErrShareAlreadyGranted
		}
		return nil, fmt.Errorf("store.RedeemCode: accept: %w", err)
	}
	return grants, nil
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

// alreadyRedeemed serves the idempotent re-redeem: rows this same person
// already accepted under this same code. Returns ErrShareNotFound when there
// are none, which is the same answer an unknown or expired code gets.
func (r *VehicleShareRepo) alreadyRedeemed(ctx context.Context, tx pgx.Tx, code, redeemerID string) ([]ShareGrant, error) {
	rows, err := tx.Query(ctx, queryAcceptedSharesByCodeAndUser, code, redeemerID)
	if err != nil {
		return nil, fmt.Errorf("store.RedeemCode: idempotency lookup: %w", err)
	}
	defer rows.Close()

	grants, err := scanGrants(rows)
	if err != nil {
		return nil, fmt.Errorf("store.RedeemCode: idempotency lookup: %w", err)
	}
	if len(grants) == 0 {
		return nil, ErrShareNotFound
	}
	return grants, nil
}

// scanGrants reads (vehicle_id, owner_user_id, allow_rides) triples — the FLAGS
// the redemption wrote, not the preset that produced them (MYR-369).
func scanGrants(rows pgx.Rows) ([]ShareGrant, error) {
	var out []ShareGrant
	for rows.Next() {
		var g ShareGrant
		if err := rows.Scan(&g.VehicleID, &g.OwnerUserID, &g.AllowRides); err != nil {
			return nil, fmt.Errorf("scan share grant: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate share grants: %w", err)
	}
	return out, nil
}

// SharedVehicleIDs and its statement queryAcceptedShareVehicleIDs were DELETED
// in MYR-369. They computed "the vehicles somebody shared with this person" and
// had no non-test caller on any branch: the real access set is assembled in ONE
// statement by internal/auth.queryUserVehicleIDs, which UNIONs owned and shared
// in the database rather than merging two reads in Go. Keeping a second, dead
// spelling of the access set was worse than useless — its comment described it
// as the set "the catalog, the snapshot, the WebSocket handshake, the drives
// surfaces and the rides surfaces all resolve through", which was false for all
// five, so a reader auditing suspension was pointed at a statement no request
// ever executed. The tests that used it as an access-set oracle now assert
// against auth.GetUserVehicles, which is the statement production actually runs.

// ShareGrantFor resolves the CAPABILITY FLAGS one person holds over one vehicle
// (MYR-369). Returns ErrShareNotFound when there is no accepted grant OR when
// the grant is suspended — callers MUST treat both as "no access", and the two
// are deliberately indistinguishable.
//
// The bool is the ride capability. It is only ever true for a LIVE grant, since
// the statement returns no row for a suspended one, so a caller cannot read a
// capability off a paused grant even by mistake.
func (r *VehicleShareRepo) ShareGrantFor(ctx context.Context, userID, vehicleID string) (bool, error) {
	var allowRides bool
	err := r.pool.QueryRow(ctx, queryAcceptedShareGrant, vehicleID, userID).Scan(&allowRides)
	switch {
	case err == nil:
		return allowRides, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, ErrShareNotFound
	default:
		return false, fmt.Errorf("store.ShareGrantFor(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
}

// RiderMayRequestRides reports whether riderID may still ride in vehicleID
// (MYR-369): true when they OWN the car, or hold a LIVE accepted grant carrying
// the ride capability.
//
// The reservation sweeper's seam. Returns a real error on a transport failure
// rather than false, so the sweeper can HOLD the reservation — "unknown" and
// "not permitted" must not collapse into the same answer on a path where the
// alternative to holding is an irreversible claim.
func (r *VehicleShareRepo) RiderMayRequestRides(ctx context.Context, riderID, vehicleID string) (bool, error) {
	var permitted bool
	if err := r.pool.QueryRow(ctx, queryRiderMayRequestRides, riderID, vehicleID).Scan(&permitted); err != nil {
		return false, fmt.Errorf("store.RiderMayRequestRides(rider=%s, vehicle=%s): %w", riderID, vehicleID, err)
	}
	return permitted, nil
}

// OwnerFirstName resolves the sharing owner's FIRST NAME for the redeemer's
// success screen ("You can now ride in {owner}'s Tesla").
//
// Deliberately narrow — first name only, no surname, no email, no user id: the
// P1 first-names-only policy that governs the push payloads and
// RideRequest.requesterName. The ladder is name → email local-part → "Owner",
// so the result is never empty and the caller never needs an absent-name
// branch. The resolved value is P1 and is never logged.
func (r *VehicleShareRepo) OwnerFirstName(ctx context.Context, ownerUserID string) (string, error) {
	var name, email *string
	err := r.pool.QueryRow(ctx, queryOwnerFirstNameSources, ownerUserID).Scan(&name, &email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store.OwnerFirstName(owner=%s): %w", ownerUserID, err)
	}
	return firstNameFrom(name, email), nil
}

// shareOwnerFallbackName is the stable literal used when the owner's row
// carries neither a usable name nor a usable email. Mirrors the "Rider"
// fallback on the ride surface.
const shareOwnerFallbackName = "Owner"

// firstNameFrom applies the name → email-local-part → literal ladder.
func firstNameFrom(name, email *string) string {
	if name != nil {
		if first := strings.Fields(*name); len(first) > 0 {
			return first[0]
		}
	}
	if email != nil {
		if local, _, ok := strings.Cut(*email, "@"); ok && local != "" {
			return local
		}
	}
	return shareOwnerFallbackName
}
