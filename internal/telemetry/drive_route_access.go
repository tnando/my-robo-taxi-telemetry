package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// WHO MAY READ ONE DRIVE'S ROUTE — the §7.4 gate, split from
// drive_route_handler.go so both stay inside the 300-line cap, and following
// the sibling-file pattern drive_route_options.go already established.
//
// The route polyline is the most locating thing this platform stores: not where
// a car is, but everywhere it has been. Its gate is the same three-way
// resolution the §7.2 and §7.3 gates run, and it is worth reading on its own
// page for that reason alone.

func (h *DriveRouteHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, driveID, vehicleID, startTime, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// The drive resolved but its vehicle did not — inconsistent
			// data, not a client error.
			h.logger.Error("drive route: drive's vehicle not found",
				slog.String("drive_id", driveID),
				slog.String("vehicle_id", vehicleID),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return false
		}
		h.logger.Error("drive route: vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
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
			// share: a trip window this caller was a participant of, covering
			// the instant THIS drive began.
			//
			// THE TWO REFUSALS ARE DIFFERENT ANSWERS TO DIFFERENT QUESTIONS.
			// A caller with no trip window at all is asking "may I read this
			// car's history", and the answer stays 403 — the pre-MYR-602
			// behaviour, unchanged. A caller who IS a participant and asked
			// for a drive outside their window gets 404: the window is the
			// entire extent of what they were told about this car, and a 403
			// would confirm it made a journey on a day they were not part of.
			admission := resolveTripDriveAdmission(ctx, h.trips, h.logger, "drive route", userID, vehicleID)
			if !admission.participant() {
				denyVehicleAccess(w, h.logger, "drive route", vehicleID, userID)
				return false
			}
			startedAt, parsed := parseDriveStartTime(startTime)
			if !parsed || !admission.covers(startedAt) {
				// An unparseable startTime is admitted to NOBODY through a
				// trip: the window test cannot be evaluated, and the safe
				// answer for an unevaluable access check is denial.
				denyDriveOutsideTripWindow(w, h.logger, "drive route", driveID, userID)
				return false
			}
			return true
		}
		h.logger.Error("drive route: access resolution failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}

	return true
}
