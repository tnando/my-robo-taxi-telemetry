package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VehicleShareRepo is the go_vehicle_shares repository (MYR-184) — the owner
// side of vehicle sharing: mint an invite, list a car's invites and viewers,
// revoke a grant, resend a code. The rider side (redeem) lives in
// vehicle_share_redeem.go on the same type.
//
// NOTHING in this file logs a `label` or a `code`: both are P1
// (data-classification.md §1.15) and rows are identified in logs and errors by
// their id.
type VehicleShareRepo struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewVehicleShareRepo builds the sharing repository over the given pool.
func NewVehicleShareRepo(pool *pgxpool.Pool, logger *slog.Logger) *VehicleShareRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &VehicleShareRepo{pool: pool, logger: logger}
}

// maxShareCodeMintAttempts bounds the collision-retry loop. Each attempt has a
// ~1-in-2.2-billion chance of colliding with a live pending code, so exceeding
// three attempts means something is wrong with the entropy source, not that we
// got unlucky.
const maxShareCodeMintAttempts = 3

// CreateInvite mints ONE code and creates one pending row per vehicle, all
// sharing it, then returns the row for in.PathVehicleID.
//
// The whole thing is one transaction: an invite that granted three of four
// requested cars would be a silent partial share, and the redeemer would be
// told they had access to a car nobody granted. Ownership of EVERY requested
// vehicle is verified inside that transaction against the authoritative
// relation — the handler's own check of the path vehicle is not trusted to
// cover the rest of the set.
func (r *VehicleShareRepo) CreateInvite(ctx context.Context, in CreateShareInviteInput) (VehicleShare, error) {
	if err := validateCreateInput(in); err != nil {
		return VehicleShare{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s): begin: %w", in.OwnerUserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := verifyOwnsAll(ctx, tx, "store.CreateInvite", in.OwnerUserID, in.VehicleIDs); err != nil {
		return VehicleShare{}, err
	}

	code, err := mintUnusedShareCode(ctx, tx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s): %w", in.OwnerUserID, err)
	}

	// NORMALIZE AT THE WRITE BOUNDARY (MYR-369). An un-updated client may
	// still submit the retired live_history preset, which validation above
	// deliberately accepts; it is collapsed to 'live' HERE, once, so the
	// value never enters the database again and no read path has to remember
	// to translate it. The row the caller gets back carries the normalized
	// value, so a client that assumed its input round-trips learns otherwise
	// from the response rather than from a later surprise.
	permission := NormalizeSharePermission(in.Permission)
	allowRides := grantAllowsRides(permission)

	var pathRow VehicleShare
	for _, vehicleID := range in.VehicleIDs {
		id := newProvisionID()
		var createdAt, expiresAt time.Time
		if err := tx.QueryRow(ctx, queryInsertShare,
			id, vehicleID, in.OwnerUserID, in.Label, permission, code, allowRides,
		).Scan(&createdAt, &expiresAt); err != nil {
			return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s, vehicle=%s): insert: %w",
				in.OwnerUserID, vehicleID, err)
		}
		if vehicleID == in.PathVehicleID {
			pathRow = VehicleShare{
				ID: id, VehicleID: vehicleID, OwnerUserID: in.OwnerUserID,
				Label: in.Label, Permission: permission, AllowRides: allowRides, Code: code,
				Status: ShareStatusPending, CreatedAt: createdAt, ExpiresAt: expiresAt,
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s): commit: %w", in.OwnerUserID, err)
	}
	return pathRow, nil
}

// validateCreateInput rejects a malformed create before it reaches the
// database. The handler validates the same things to produce a good 400; this
// is the repository refusing to write a row it cannot justify.
func validateCreateInput(in CreateShareInviteInput) error {
	switch {
	case strings.TrimSpace(in.OwnerUserID) == "":
		return errors.New("store.CreateInvite: empty owner id")
	case strings.TrimSpace(in.Label) == "":
		return errors.New("store.CreateInvite: empty label")
	case !ValidSharePermission(in.Permission):
		return fmt.Errorf("store.CreateInvite: invalid permission %q", in.Permission)
	case len(in.VehicleIDs) == 0:
		return errors.New("store.CreateInvite: empty vehicle set")
	}
	for _, id := range in.VehicleIDs {
		if id == in.PathVehicleID {
			return nil
		}
	}
	return fmt.Errorf("store.CreateInvite: vehicle set omits path vehicle %s", in.PathVehicleID)
}

// mintUnusedShareCode draws codes until one is not already backing a live
// pending invite.
//
// The check and the insert are not atomic against a concurrent create that
// happens to draw the SAME code in the same instant — there is no unique
// constraint to lean on, because a multi-vehicle invite legitimately shares one
// code across N rows. The residual race is a ~1-in-2.2-billion draw colliding
// inside a few milliseconds, and the redeem path refuses outright (never
// guesses) if it ever does resolve a code to two owners. See
// ErrShareCodeCollision.
func mintUnusedShareCode(ctx context.Context, tx pgx.Tx) (string, error) {
	for attempt := 0; attempt < maxShareCodeMintAttempts; attempt++ {
		code, err := newShareCode()
		if err != nil {
			return "", err
		}
		var inUse bool
		if err := tx.QueryRow(ctx, queryShareCodeInUse, code).Scan(&inUse); err != nil {
			return "", fmt.Errorf("code-in-use probe: %w", err)
		}
		if !inUse {
			return code, nil
		}
	}
	// The value is never reported — only the failure to find a free one.
	return "", errors.New("could not mint an unused invite code")
}

// ListInvitesForVehicle returns the owner's pending invites and accepted
// grants for one vehicle, newest first. Revoked tombstones are excluded by the
// query. Scoped to ownerUserID: a caller who does not own the vehicle gets an
// empty list, never another owner's rows.
func (r *VehicleShareRepo) ListInvitesForVehicle(ctx context.Context, vehicleID, ownerUserID string) ([]VehicleShare, error) {
	rows, err := r.pool.Query(ctx, queryListSharesByVehicle, vehicleID, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("store.ListInvitesForVehicle(vehicle=%s): %w", vehicleID, err)
	}
	defer rows.Close()

	out := make([]VehicleShare, 0, 8)
	for rows.Next() {
		share, err := scanShare(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListInvitesForVehicle(vehicle=%s): %w", vehicleID, err)
		}
		out = append(out, share)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListInvitesForVehicle(vehicle=%s): iterate: %w", vehicleID, err)
	}
	return out, nil
}

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

// scanShare reads one full row in the shareColumns order. rowScanner (declared
// in vehicle_repo_scan.go) is the shared pgx.Row / pgx.Rows surface, so this
// serves both the single-row RETURNING path and the list iteration.
func scanShare(row rowScanner) (VehicleShare, error) {
	var s VehicleShare
	// The ladder's RAW answer (a full display name, or NULL). Reduced to a first
	// name below rather than scanned straight onto the struct, so the only value
	// that ever leaves this repository is the first-names-only one — the same
	// discipline the vehicle catalog's owner name keeps.
	var acceptedByName *string
	// `expires_at` is NULLABLE since migration 0052: a row born ACCEPTED by
	// §7.5.8 extend has no deadline because it has no credential. Scanned
	// through a pointer and flattened to the ZERO time, which is the absence
	// every reader already handles — `toShareInviteWire` omits `expiresAt`
	// when it is zero, and the pending branch that reads it cannot run on a
	// row that has none (the migration's CHECK requires both on a pending
	// row). Widening the struct field to *time.Time instead would have made
	// every caller learn a nil case for a value only one status ever has.
	var expiresAt *time.Time
	err := row.Scan(
		&s.ID, &s.VehicleID, &s.OwnerUserID, &s.Label, &s.Permission,
		&s.AllowRides, &s.SuspendedAt,
		&s.Code, &s.Status, &s.CreatedAt, &expiresAt, &s.AcceptedAt,
		&s.AcceptedByUserID, &s.RevokedAt,
		&acceptedByName,
	)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("scan vehicle share: %w", err)
	}
	if expiresAt != nil {
		s.ExpiresAt = *expiresAt
	}
	s.AcceptedByName = ownerFirstNameToken(acceptedByName)
	return s, nil
}
