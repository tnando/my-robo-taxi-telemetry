package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// WHO MAY READ ONE DRIVE — the §7.3 gate, split from drive_detail_handler.go
// so both stay inside the 300-line cap, and following the sibling-file pattern
// drive_route_options.go already established in this package.
//
// It is lifted out for the reason its §7.2 twin is: MYR-602 turned it from an
// ownership check into a three-way resolution with a WINDOW bound, and the
// 404-not-403 rule it enforces is the thing worth reading without the
// pagination and the marshalling around it.

// verifyOwnership resolves the caller's access to the drive's vehicle: the
// OWNER, and nobody else (MYR-369 — no share of any shape opens the drives
// surfaces), plus the one MYR-602 trip-window exception below. Returns true on
// success; on failure it writes an HTTP error and returns false.
//
// THREE DIFFERENT FAILURES, THREE DIFFERENT ANSWERS. A drive pointing at a
// missing vehicle is a data-integrity fault (500), distinct from an ownership
// mismatch (403 vehicle_not_owned), which is in turn distinct from a
// participant's drive outside their window (404) — and, since MYR-614, from a
// drive whose startTime cannot be parsed at all (500 again, because the gate
// could not evaluate its own question).
func (h *DriveDetailHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, driveID, vehicleID, startTime, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// The drive resolved but its vehicle did not — inconsistent
			// data, not a client error.
			h.logger.Error("drive detail: drive's vehicle not found",
				slog.String("drive_id", driveID),
				slog.String("vehicle_id", vehicleID),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return false
		}
		h.logger.Error("drive detail: vehicle lookup failed",
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
			admission := resolveTripDriveAdmission(ctx, h.trips, h.logger, "drive detail", userID, vehicleID)
			if !admission.participant() {
				denyVehicleAccess(w, h.logger, "drive detail", vehicleID, userID)
				return false
			}
			startedAt, parsed := parseDriveStartTime(startTime)
			if !parsed {
				// MYR-614: SEPARATED FROM THE REFUSAL BELOW. An unparseable
				// startTime is admitted to nobody either way, but it is a
				// server data fault rather than a legitimate "outside your
				// window", and reporting it as the latter is what let a whole
				// surface fail silently for every participant. 500 + an
				// Error log; see failDriveStartTimeUnreadable.
				failDriveStartTimeUnreadable(w, h.logger, "drive detail", driveID, vehicleID, userID, startTime)
				return false
			}
			if !admission.covers(startedAt) {
				denyDriveOutsideTripWindow(w, h.logger, "drive detail", driveID, userID)
				return false
			}
			return true
		}
		h.logger.Error("drive detail: access resolution failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}

	return true
}
