package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// THE TWO WAYS A GRANT ENDS, and everything they must end with it.
//
//	RevokeInvite         the OWNER cuts somebody off        (rest-api.md §7.5.3)
//	LeaveVehicleShares   the GRANTEE walks away             (§7.5.7, MYR-469)
//
// Split out of vehicle_share_repo.go in the MYR-618 review round, when both
// gained a transaction and the file passed the 300-line cap. The seam is a real
// one rather than arithmetic: everything left in the parent file MINTS or READS
// access, and these two are the only statements that take it away — which is
// also why they are the two that must not end at two different instants.
//
// ⚠ BOTH RUN THE TRIP-ROSTER CASCADE INSIDE THEIR OWN TRANSACTION.
// `removeTripParticipantsForShare` (trip_repo_end.go) stamps `left_at` on the
// person's memberships in that car's non-ended trips. Before this round nothing
// in production called it at all, so a severed grant left the person on every
// roster on that car indefinitely: the owner's trip card kept listing somebody
// who could see nothing, and the participant count lied.
//
// **THE CASCADE IS COSMETIC AND THE TRANSACTION IS NOT AN ADMISSION OTHERWISE.**
// Trip access cannot outlive the share because every access query re-joins the
// live grant — docs/architecture/trips.md §6 spends a section refusing to let
// this be read as the enforcement. The transaction is here because a repair
// that lands separately from the thing it repairs can land late, or not at all,
// and then the roster is wrong for a window nobody can bound.
//
// SUSPENSION IS NOT HERE, and that is the point of the file's title. A suspend
// is REVERSIBLE — the owner is pausing somebody, not removing them — so it
// touches no roster: stamping `left_at` would turn a pause into a departure
// that un-suspending could not undo. What stops a suspended grant-holder acting
// on a trip is the live-grant re-join in the role probe (`tripMemberRoleExpr`);
// what stops them seeing anything is the four access legs.
//
// NOTHING HERE LOGS A `label` OR A `code`: both are P1
// (data-classification.md §1.15), and rows are named in logs by their id.

// RevokeInvite tombstones an invite (pending → revoked) or an accepted grant
// (accepted → revoked). IDEMPOTENT: revoking an already-revoked row that
// belongs to the caller succeeds silently, so a retried DELETE is safe.
//
// Returns ErrShareNotFound when the row does not exist OR belongs to somebody
// else — the two are deliberately indistinguishable, so the endpoint cannot be
// used to probe for the existence of other people's invites.
//
// The returned RevokedShare names who lost access and to which vehicle. The
// viewer id is empty when the row was still pending (nobody held access) and
// on the idempotent already-revoked path (this call removed nothing). The
// caller uses it to bust that person's cached access set and to close their
// live socket for that car: without the first, a revoked viewer keeps
// resolving the vehicle until the cache TTL lapses; without the second, an
// already-open WebSocket keeps streaming its GPS until reconnect
// (websocket-protocol.md §10 DV-09).
//
// ⚠ IT NOW RUNS IN A TRANSACTION, AND THE TRIP-ROSTER CASCADE RUNS INSIDE IT
// (MYR-618 review round). `TripRepo.RemoveParticipantsForShare` existed from
// MYR-602 and NOTHING CALLED IT: a revoked grant-holder stayed on every trip
// roster on that car indefinitely, so an owner's trip card kept listing
// somebody who could see nothing and the participant count lied.
//
// **THE CASCADE IS COSMETIC AND THE TRANSACTION IS NOT AN ADMISSION OTHERWISE.**
// Trip access cannot outlive the share because every access query re-joins the
// live grant — see trips.md §6, which spends a section refusing to let this be
// read as the enforcement. The transaction is here because a repair that lands
// separately from the thing it repairs can land LATE, or not at all, and then
// the roster is wrong for a window nobody can bound.
func (r *VehicleShareRepo) RevokeInvite(ctx context.Context, inviteID, ownerUserID string) (RevokedShare, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RevokedShare{}, fmt.Errorf("store.RevokeInvite(invite=%s): begin: %w", inviteID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	revoked, err := revokeShareInTx(ctx, tx, inviteID, ownerUserID)
	if err != nil {
		return RevokedShare{}, err
	}
	// Only the arm that ACTUALLY tombstoned an accepted grant has a person to
	// cascade for. A pending row had no viewer, and the idempotent
	// already-revoked path removed nothing this call is responsible for.
	if revoked.ViewerUserID != "" {
		n, cErr := removeTripParticipantsForShare(ctx, tx, revoked.VehicleID, revoked.ViewerUserID)
		if cErr != nil {
			return RevokedShare{}, fmt.Errorf("store.RevokeInvite(invite=%s): %w", inviteID, cErr)
		}
		if n > 0 {
			r.logger.Info("trip rosters repaired after share revoke",
				"invite_id", inviteID, "vehicle_id", revoked.VehicleID, "memberships", n)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RevokedShare{}, fmt.Errorf("store.RevokeInvite(invite=%s): commit: %w", inviteID, err)
	}
	return revoked, nil
}

// revokeShareInTx is the tombstone flip and its two zero-row arms, unchanged
// from the pre-transaction version except for the querier it takes.
func revokeShareInTx(ctx context.Context, tx pgx.Tx, inviteID, ownerUserID string) (RevokedShare, error) {
	var revoked RevokedShare
	switch err := tx.QueryRow(ctx, queryRevokeShare, inviteID, ownerUserID).
		Scan(&revoked.ViewerUserID, &revoked.VehicleID); {
	case err == nil:
		return revoked, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return RevokedShare{}, fmt.Errorf("store.RevokeInvite(invite=%s): %w", inviteID, err)
	}

	// Zero rows: either already revoked (idempotent success) or not ours.
	var status string
	switch err := tx.QueryRow(ctx, queryShareExistsForOwner, inviteID, ownerUserID).Scan(&status); {
	case err == nil:
		// status is necessarily 'revoked' — the UPDATE covered the rest.
		// Nothing to report: the access this call would have removed was
		// already gone, so there is no cache to bust and no socket to close.
		return RevokedShare{}, nil
	case errors.Is(err, pgx.ErrNoRows):
		return RevokedShare{}, ErrShareNotFound
	default:
		return RevokedShare{}, fmt.Errorf("store.RevokeInvite(invite=%s): probe: %w", inviteID, err)
	}
}

// ShareLeaveResult is what a rider's leave found (MYR-469).
type ShareLeaveResult int

const (
	// ShareLeaveDone — at least one accepted grant was tombstoned (or there
	// was nothing to leave; both are the caller's desired end state).
	ShareLeaveDone ShareLeaveResult = iota
	// ShareLeaveRefusedLiveRide — the caller has a live ride on this vehicle,
	// so the grant stays until the ride ends or is cancelled.
	ShareLeaveRefusedLiveRide
)

// LeaveVehicleShares tombstones every accepted grant viewerUserID redeemed on
// vehicleID (MYR-469 — the rider-side mirror of RevokeInvite). Idempotent: a
// vehicle the caller never had, or already left, is ShareLeaveDone with zero
// rows — deliberately indistinguishable, so the endpoint cannot be used to
// probe which vehicles exist.
//
// ⚠ THE TRIP-ROSTER CASCADE RUNS IN THE SAME TRANSACTION (MYR-618 review
// round), for the reason RevokeInvite's does: the two ways a grant ends must
// end the rosters drawn from it at the same instant. This is the arm where the
// argument is sharpest — the person is walking away themselves, so a roster
// still naming them is the platform telling their friends they are on a trip
// they deliberately left.
//
// It stamps `left_at` and NOT `removed_by_owner`: this statement serves both
// severing paths and cannot tell an owner's revoke from a grantee's own exit
// (see queryLeaveTripByShare). Nothing turns on it, because the add's
// live-grant predicate refuses a person whose grant is gone long before the
// marker would be read.
func (r *VehicleShareRepo) LeaveVehicleShares(ctx context.Context, vehicleID, viewerUserID string) (ShareLeaveResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ShareLeaveDone, fmt.Errorf("store.LeaveVehicleShares(vehicle=%s): begin: %w", vehicleID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := r.leaveVehicleSharesInTx(ctx, tx, vehicleID, viewerUserID)
	if err != nil {
		return ShareLeaveDone, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ShareLeaveDone, fmt.Errorf("store.LeaveVehicleShares(vehicle=%s): commit: %w", vehicleID, err)
	}
	return result, nil
}

func (r *VehicleShareRepo) leaveVehicleSharesInTx(
	ctx context.Context, tx pgx.Tx, vehicleID, viewerUserID string,
) (ShareLeaveResult, error) {
	tag, err := tx.Exec(ctx, queryLeaveVehicleShares, vehicleID, viewerUserID)
	if err != nil {
		return ShareLeaveDone, fmt.Errorf("store.LeaveVehicleShares(vehicle=%s): %w", vehicleID, err)
	}
	if tag.RowsAffected() > 0 {
		n, cErr := removeTripParticipantsForShare(ctx, tx, vehicleID, viewerUserID)
		if cErr != nil {
			return ShareLeaveDone, fmt.Errorf("store.LeaveVehicleShares(vehicle=%s): %w", vehicleID, cErr)
		}
		if n > 0 {
			r.logger.Info("trip rosters repaired after grantee left",
				"vehicle_id", vehicleID, "memberships", n)
		}
		return ShareLeaveDone, nil
	}
	// Zero rows: nothing accepted (ordinary, idempotent) — or the guard held.
	// Nothing was severed, so there is nothing to cascade.
	var one int
	switch err := tx.QueryRow(ctx, queryViewerLeaveRefused, vehicleID, viewerUserID).Scan(&one); {
	case err == nil:
		return ShareLeaveRefusedLiveRide, nil
	case errors.Is(err, pgx.ErrNoRows):
		return ShareLeaveDone, nil
	default:
		return ShareLeaveDone, fmt.Errorf("store.LeaveVehicleShares(vehicle=%s): probe: %w", vehicleID, err)
	}
}
