package main

import (
	"context"
	"errors"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// Boundary adapters for the MYR-184 vehicle-sharing surface.
//
// internal/telemetry does not import internal/store, so this is where
// store.VehicleShare rows become telemetry.ShareInviteRow and where the store's
// typed sentinels become the handler layer's. The translation is deliberately
// EXPLICIT rather than a shared error package: the handler layer must not be
// able to accidentally depend on a store-internal error, and each mapping below
// is a documented status decision.

// shareInviteAdapter binds store.VehicleShareRepo to telemetry.ShareInviteStore.
type shareInviteAdapter struct {
	repo *store.VehicleShareRepo
}

// CreateInvite mints an invite and returns the path vehicle's row.
func (a *shareInviteAdapter) CreateInvite(ctx context.Context, in telemetry.ShareInviteCreateInput) (telemetry.ShareInviteRow, error) {
	row, err := a.repo.CreateInvite(ctx, store.CreateShareInviteInput{
		OwnerUserID:   in.OwnerUserID,
		PathVehicleID: in.PathVehicleID,
		VehicleIDs:    in.VehicleIDs,
		Label:         in.Label,
		Permission:    in.Permission,
	})
	if err != nil {
		return telemetry.ShareInviteRow{}, translateShareError(err)
	}
	return toShareInviteRow(&row), nil
}

// ListInvitesForVehicle returns the owner's live rows for one vehicle.
func (a *shareInviteAdapter) ListInvitesForVehicle(ctx context.Context, vehicleID, ownerUserID string) ([]telemetry.ShareInviteRow, error) {
	rows, err := a.repo.ListInvitesForVehicle(ctx, vehicleID, ownerUserID)
	if err != nil {
		return nil, translateShareError(err)
	}
	out := make([]telemetry.ShareInviteRow, 0, len(rows))
	for i := range rows {
		out = append(out, toShareInviteRow(&rows[i]))
	}
	return out, nil
}

// RevokeInvite tombstones a row and reports whose access ended, and on which
// vehicle — the handler needs both to bust the right cache entry and close the
// right live socket.
func (a *shareInviteAdapter) RevokeInvite(ctx context.Context, inviteID, ownerUserID string) (telemetry.RevokedGrant, error) {
	revoked, err := a.repo.RevokeInvite(ctx, inviteID, ownerUserID)
	if err != nil {
		return telemetry.RevokedGrant{}, translateShareError(err)
	}
	return telemetry.RevokedGrant{
		ViewerUserID: revoked.ViewerUserID,
		VehicleID:    revoked.VehicleID,
	}, nil
}

// PatchInvite applies an owner edit to one accepted grant (MYR-369) and reports
// the grantee whose access set changed, so the caller can bust their cache.
//
// The grantee id comes from the row the UPDATE returned, not from a second
// lookup: it is whoever holds the grant that was just edited, read in the same
// statement that edited it, so a concurrent revoke cannot make the two disagree.
func (a *shareInviteAdapter) PatchInvite(
	ctx context.Context,
	inviteID, ownerUserID string,
	patch telemetry.ShareInvitePatch,
) (telemetry.ShareInviteRow, string, error) {
	row, err := a.repo.PatchInvite(ctx, store.PatchShareInviteInput{
		InviteID:    inviteID,
		OwnerUserID: ownerUserID,
		AllowRides:  patch.AllowRides,
		Suspended:   patch.Suspended,
	})
	if err != nil {
		return telemetry.ShareInviteRow{}, "", translateShareError(err)
	}
	return toShareInviteRow(&row), row.AcceptedByUserID, nil
}

// ResendInvite re-mints the code on a pending row.
func (a *shareInviteAdapter) ResendInvite(ctx context.Context, inviteID, ownerUserID string) (telemetry.ShareInviteRow, error) {
	row, err := a.repo.ResendInvite(ctx, inviteID, ownerUserID)
	if err != nil {
		return telemetry.ShareInviteRow{}, translateShareError(err)
	}
	return toShareInviteRow(&row), nil
}

// OwnerFirstName resolves the calling owner's first name for the `from` half of
// a signed share link (MYR-368). Same repository method the redeem side uses:
// one ladder, one policy, no second definition of "first name" to drift.
// LeaveVehicleShares — MYR-469, the rider-side mirror of RevokeInvite.
func (a *shareInviteAdapter) LeaveVehicleShares(ctx context.Context, vehicleID, viewerUserID string) (telemetry.ShareLeaveOutcome, error) {
	result, err := a.repo.LeaveVehicleShares(ctx, vehicleID, viewerUserID)
	if err != nil {
		return telemetry.ShareLeaveDone, err
	}
	if result == store.ShareLeaveRefusedLiveRide {
		return telemetry.ShareLeaveRefusedLiveRide, nil
	}
	return telemetry.ShareLeaveDone, nil
}

func (a *shareInviteAdapter) OwnerFirstName(ctx context.Context, ownerUserID string) (string, error) {
	name, err := a.repo.OwnerFirstName(ctx, ownerUserID)
	if err != nil {
		return "", translateShareError(err)
	}
	return name, nil
}

// shareRedeemAdapter binds store.VehicleShareRepo to telemetry.ShareRedeemStore.
type shareRedeemAdapter struct {
	repo *store.VehicleShareRepo
}

// RedeemCode accepts every pending row for a code atomically.
func (a *shareRedeemAdapter) RedeemCode(ctx context.Context, code, redeemerID string) ([]telemetry.ShareGrantRow, error) {
	grants, err := a.repo.RedeemCode(ctx, code, redeemerID)
	if err != nil {
		return nil, translateShareError(err)
	}
	out := make([]telemetry.ShareGrantRow, 0, len(grants))
	for _, g := range grants {
		out = append(out, telemetry.ShareGrantRow{
			VehicleID:   g.VehicleID,
			OwnerUserID: g.OwnerUserID,
			AllowRides:  g.AllowRides,
		})
	}
	return out, nil
}

// OwnerFirstName resolves the sharing owner's first name.
func (a *shareRedeemAdapter) OwnerFirstName(ctx context.Context, ownerUserID string) (string, error) {
	return a.repo.OwnerFirstName(ctx, ownerUserID)
}

// shareReaderAdapter binds store.VehicleShareRepo to
// telemetry.VehicleShareReader — the per-vehicle tier lookup every read gate
// consults for a non-owner.
type shareReaderAdapter struct {
	repo *store.VehicleShareRepo
}

// ShareGrantFor resolves the caller's CAPABILITY SET over one vehicle
// (MYR-369). The store's ErrShareNotFound already wraps sdk.ErrNotFound, which
// is the "no grant" signal the gate checks, so it passes through untranslated —
// and the store returns it for a SUSPENDED grant too, which is why a suspended
// viewer is refused by every gate without any of them naming suspension.
//
// The returned grant is always Active: the store's statement excludes suspended
// rows, so there is no paused grant for this adapter to hand a gate. No parse
// step remains — the value crossing the boundary is a bool, not a string that
// could carry a tier the enum has never heard of.
func (a *shareReaderAdapter) ShareGrantFor(ctx context.Context, userID, vehicleID string) (auth.ShareGrant, error) {
	allowRides, err := a.repo.ShareGrantFor(ctx, userID, vehicleID)
	if err != nil {
		return auth.ShareGrant{}, err
	}
	return auth.ShareGrant{AllowRides: allowRides}, nil
}

// toShareInviteRow drops the two server-only columns (accepted_by_user_id,
// revoked_at) that the owner-facing wire shape deliberately never carries — and
// since MYR-581 carries the resolved NAME of that same accepting account, which
// is the point: the owner may know who holds their grant, not the opaque id of
// the account that holds it.
func toShareInviteRow(row *store.VehicleShare) telemetry.ShareInviteRow {
	return telemetry.ShareInviteRow{
		ID:         row.ID,
		VehicleID:  row.VehicleID,
		Label:      row.Label,
		Permission: row.Permission,
		Grant: auth.ShareGrant{
			AllowRides: row.AllowRides,
			// The owner's own listing DOES show suspended grants — it is
			// the only place they can be seen and lifted — so unlike every
			// viewer-facing path this conversion must carry the flag
			// rather than assume it clear.
			Suspended: row.SuspendedAt != nil,
		},
		Code:       row.Code,
		Status:     row.Status,
		CreatedAt:  row.CreatedAt,
		ExpiresAt:  row.ExpiresAt,
		AcceptedAt: row.AcceptedAt,
		// MYR-581: WHO ACTUALLY HOLDS THE GRANT, already reduced to a first name
		// by the store. It crosses this boundary where `AcceptedByUserID` does
		// NOT — the id stays server-side (see this function's doc), and the name
		// is the owner-facing projection of it.
		AcceptedByName: row.AcceptedByName,
	}
}

// translateShareError maps store sentinels onto the handler layer's.
//
// store.ErrShareNotFound is NOT listed: it already wraps sdk.ErrNotFound, which
// is the signal the handlers check, and re-wrapping it would only create a
// second thing to keep in sync. Everything unrecognized passes through and
// becomes a 500 — the safe default for an error nobody has classified.
func translateShareError(err error) error {
	switch {
	case errors.Is(err, store.ErrShareVehicleNotOwned):
		return telemetry.ErrShareVehicleNotOwned
	case errors.Is(err, store.ErrShareNotPending):
		return telemetry.ErrShareNotPending
	case errors.Is(err, store.ErrShareNotAccepted):
		return telemetry.ErrShareNotAccepted
	case errors.Is(err, store.ErrShareSelfRedeem):
		return telemetry.ErrShareSelfRedeem
	case errors.Is(err, store.ErrShareAlreadyGranted):
		return telemetry.ErrShareAlreadyGranted
	// MYR-609 §7.5.8 — the three refusals only extend produces. Each is a
	// DISTINCT 409 with its own message; folding any of them into
	// ErrShareAlreadyGranted would hand a client the `already_shared`
	// sub-code, which the contract tells it to render as SUCCESS, for a call
	// that granted nothing.
	case errors.Is(err, store.ErrShareSourceSuspended):
		return telemetry.ErrShareSourceSuspended
	case errors.Is(err, store.ErrShareTargetSuspended):
		return telemetry.ErrShareTargetSuspended
	case errors.Is(err, store.ErrShareGranteeLeft):
		return telemetry.ErrShareGranteeLeft
	default:
		return err
	}
}
