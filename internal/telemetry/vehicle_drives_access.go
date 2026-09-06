package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// WHO MAY READ A CAR'S DRIVES — the §7.2 gate, split from
// vehicle_drives_handler.go so both stay inside the 300-line cap.
//
// It earns its own page because MYR-602 made it the most argued few lines on
// the surface: the drive list was owner-only, then owner-or-share-holder, and
// is now owner-or-share-holder-or-trip-participant with a WINDOW bound that a
// share alone does not carry. The handler beside it is shape and pagination;
// this is the part where getting it wrong hands somebody a record of where a
// car has been.

// authorize resolves the caller's access to the vehicle identified by
// vehicleID: the OWNER (MYR-369 — no share of any shape opens the drives
// surfaces), or, since MYR-602, a TRIP PARTICIPANT limited to their own
// windows. Returns the trip admission — empty for an owner, who needs no
// narrowing — and false after writing an HTTP error. The 404 / 403 split
// mirrors the snapshot handler: an unknown vehicle is never distinguishable
// from one the caller cannot see.
func (h *VehicleDrivesHandler) authorize(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) (tripDriveAdmission, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return tripDriveAdmission{}, false
		}
		h.logger.Error("vehicle drives: vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return tripDriveAdmission{}, false
	}

	// MYR-369: THE DRIVES SURFACES ARE OWNER-ONLY AGAIN, unconditionally.
	//
	// MYR-184 opened them to a viewer holding `live_history` or better. That
	// tier is RETIRED and the capability is removed from the product, so the
	// gate is back to what it was before sharing shipped: owner or nobody.
	// This is a DELIBERATE NARROWING — a legacy grant created at
	// `live_history` can no longer read drives, and there is no flag that
	// re-opens them. Suspension is irrelevant here for the same reason: no
	// grant of any shape passes.
	//
	// Expressed as capBase-against-the-owner rather than a bare
	// `row.UserID != userID` comparison so the denial still flows through the
	// one access helper — same 403, same log shape, same non-oracle message
	// as every other refusal — and so re-opening the surface later is a
	// one-argument change rather than a re-derivation.
	if _, err := vehicleAccessForOwnerOnly(ctx, userID, row.UserID); err != nil {
		if errors.Is(err, errNoVehicleAccess) {
			// MYR-602 ADDS EXACTLY ONE WAY PAST THAT DENIAL, and it is not a
			// share: an OPEN OR CLOSED TRIP WINDOW this caller was a
			// participant of. It buys them the drives of that window and
			// nothing else about the car's history — see trip_drive_access.go.
			//
			// The probe runs only AFTER the owner check has already failed, so
			// the owner path costs nothing, and it fails closed: no windows,
			// or a lookup error, and the original 403 stands.
			admission := resolveTripDriveAdmission(ctx, h.trips, h.logger, "vehicle drives", userID, vehicleID)
			if admission.participant() {
				return admission, true
			}
			denyVehicleAccess(w, h.logger, "vehicle drives", vehicleID, userID)
			return tripDriveAdmission{}, false
		}
		h.logger.Error("vehicle drives: access resolution failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return tripDriveAdmission{}, false
	}

	return tripDriveAdmission{}, true
}
