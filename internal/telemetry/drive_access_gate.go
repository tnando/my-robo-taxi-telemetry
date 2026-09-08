package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// WHO MAY READ ONE DRIVE — the gate BOTH §7.3 (drive detail) and §7.4 (drive
// route) run, in one function.
//
// IT USED TO BE TWO. `(*DriveDetailHandler).verifyOwnership` and
// `(*DriveRouteHandler).verifyOwnership` were byte-identical apart from their
// receiver and a surface string, in two files, edited in lockstep by every
// change that touched the rule — which is precisely the arrangement that
// produced MYR-614: two copies of one decision, and a divergence between them
// that no test crossed. The route surface refused every trip participant every
// drive for as long as it took a client to notice. Collapsing them means the
// next change to the rule cannot land on one surface only.
//
// It takes DriveAccessFacts rather than the four adjacent bare strings the two
// copies took (`driveID, vehicleID, startTime, userID`, in an order the old
// failure helper already spelled differently) — the same shared identity the
// two read models embed, so a caller cannot transpose two of them.

// verifyDriveAccess resolves the caller's access to the drive's vehicle: the
// OWNER, and nobody else (MYR-369 — no share of any shape opens the drives
// surfaces), plus the one MYR-602 trip-window exception below. Returns true on
// success; on failure it writes an HTTP error and returns false.
//
// THREE DIFFERENT FAILURES, THREE DIFFERENT ANSWERS. A drive pointing at a
// missing vehicle is a data-integrity fault (500), distinct from an ownership
// mismatch (403 vehicle_not_owned), which is in turn distinct from a
// participant's drive outside their window (404). A drive whose `startTime`
// will not parse joins that last answer on the wire and is separated from it in
// the LOG — see denyDriveWithUnreadableStartTime for why the fault may not
// reach the client.
func verifyDriveAccess(
	ctx context.Context,
	w http.ResponseWriter,
	vehicles VehicleSnapshotReader,
	trips TripDriveAdmitter,
	logger *slog.Logger,
	surface string,
	facts DriveAccessFacts,
	userID string,
) bool {
	row, err := vehicles.GetByID(ctx, facts.VehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// The drive resolved but its vehicle did not — inconsistent
			// data, not a client error.
			logger.Error(surface+": drive's vehicle not found",
				slog.String("drive_id", facts.DriveID),
				slog.String("vehicle_id", facts.VehicleID),
			)
			writeDriveAccessInternalError(w, logger)
			return false
		}
		logger.Error(surface+": vehicle lookup failed",
			slog.String("vehicle_id", facts.VehicleID),
			slog.String("error", err.Error()),
		)
		writeDriveAccessInternalError(w, logger)
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
			return admitTripParticipant(ctx, w, trips, logger, surface, facts, userID)
		}
		logger.Error(surface+": access resolution failed",
			slog.String("vehicle_id", facts.VehicleID),
			slog.String("error", err.Error()),
		)
		writeDriveAccessInternalError(w, logger)
		return false
	}

	return true
}

// admitTripParticipant is MYR-602's ONE way past the owner-only denial, and it
// is not a share: a trip window this caller was a participant of, covering the
// instant THIS drive began.
//
// THE TWO REFUSALS ARE DIFFERENT ANSWERS TO DIFFERENT QUESTIONS. A caller with
// no trip window at all is asking "may I read this car's history", and the
// answer stays 403 — the pre-MYR-602 behaviour, unchanged. A caller who IS a
// participant and asked for a drive outside their window gets 404: the window
// is the entire extent of what they were told about this car, and a 403 would
// confirm it made a journey on a day they were not part of.
func admitTripParticipant(
	ctx context.Context,
	w http.ResponseWriter,
	trips TripDriveAdmitter,
	logger *slog.Logger,
	surface string,
	facts DriveAccessFacts,
	userID string,
) bool {
	admission := resolveTripDriveAdmission(ctx, trips, logger, surface, userID, facts.VehicleID)
	if !admission.participant() {
		denyVehicleAccess(w, logger, surface, facts.VehicleID, userID)
		return false
	}
	startedAt, parsed := ParseDriveStartTime(facts.StartTime)
	if !parsed {
		// MYR-614: THE SAME 404, A DIFFERENT LOG. An unparseable startTime
		// leaves the window test unevaluable, which is a server data fault
		// — but a fault reported as a distinct STATUS would tell a
		// participant that this drive exists, which is the one thing the
		// 404-not-403 rule is here to withhold. The fault goes to the log.
		denyDriveWithUnreadableStartTime(w, logger, surface, facts, userID)
		return false
	}
	if !admission.covers(startedAt) {
		denyDriveOutsideTripWindow(w, logger, surface, facts.DriveID, userID)
		return false
	}
	return true
}

// writeDriveAccessInternalError is the gate's 500 — a SERVER-side failure the
// caller had no part in (a missing vehicle row, a lookup that errored), never a
// statement about the drive.
func writeDriveAccessInternalError(w http.ResponseWriter, logger *slog.Logger) {
	wserrors.WriteErrorEnvelope(w, logger, http.StatusInternalServerError,
		wserrors.ErrCodeInternalError, "internal error")
}
