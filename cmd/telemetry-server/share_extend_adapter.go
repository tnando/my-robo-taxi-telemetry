package main

import (
	"context"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// The §7.5.8 extend half of the vehicle-sharing boundary (MYR-609). Split from
// share_adapters.go under the 300-line rule; the translation discipline stated
// at the top of that file governs here unchanged — internal/telemetry does not
// import internal/store, so this is where the store row becomes a
// telemetry.ShareInviteRow and the store's typed sentinels become the handler
// layer's, each mapping a documented status decision rather than a shared error
// package the handler could accidentally depend on.

// ExtendShare copies an accepted grant onto another of the owner's cars
// (MYR-609) and reports the GRANTEE whose access set widened, so the handler
// can bust their cache and make the car resolvable on their next call.
//
// The grantee id comes off the row the INSERT returned — the same statement
// that created the grant — rather than from the source row read a moment
// earlier, so the id the caller busts is the id the new grant actually names.
func (a *shareInviteAdapter) ExtendShare(
	ctx context.Context, in telemetry.ShareExtendInput,
) (telemetry.ShareInviteRow, string, error) {
	row, err := a.repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID:     in.OwnerUserID,
		TargetVehicleID: in.TargetVehicleID,
		SourceShareID:   in.SourceShareID,
	})
	if err != nil {
		return telemetry.ShareInviteRow{}, "", translateShareError(err)
	}
	return toShareInviteRow(&row), row.AcceptedByUserID, nil
}
