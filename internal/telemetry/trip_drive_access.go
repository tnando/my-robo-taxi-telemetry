package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// THE MYR-602 TRIP-WINDOW GATE for the drives surfaces (§7.2, §7.3, §7.4).
//
// MYR-369 made drive history OWNER-ONLY and that still stands for `viewer` and
// for `ride_member`: no share of any shape, and no ride, opens these endpoints.
// A TRIP PARTICIPANT is the first and only exception, and it is narrow — they
// may read the drives that fall inside a window they were actually part of, and
// nothing else about the car's history.
//
// THE BOUND IS ENFORCED HERE AND IN THE STORE, NEVER BY THE FIELD MASK. The
// mask tables give `trip_participant` the same drive fields an owner sees, and
// that is correct: what differs between the roles is WHICH DRIVES they may ask
// for, never what a drive says once they may see it. A mask can hide a field
// and cannot hide a row.
//
// THE FAILURE ANSWER IS 404, NOT 403, and only for this role. A participant
// asking for a drive outside their window must not learn that the drive exists
// — the window is the whole extent of what they were told about, and a 403
// would confirm the car made a journey on a day they were not part of. The
// plain-viewer 403 is untouched: it is the answer to a DIFFERENT question
// ("may you read this car's history at all"), it predates trips, and changing
// it here would be a silent behaviour change on a surface this issue is not
// about.

// TripDriveWindow is one (from, to) span a trip admits its participant to.
// Both bounds are INCLUSIVE, matching store.Trip.Window(): a drive that began
// exactly at the closing instant is a drive of that trip.
type TripDriveWindow struct {
	From time.Time
	To   time.Time
}

// Covers reports whether a drive that began at startedAt falls in this window.
func (w TripDriveWindow) Covers(startedAt time.Time) bool {
	return !startedAt.Before(w.From) && !startedAt.After(w.To)
}

// TripDriveAdmitter is the consumer-site interface the drives handlers use to
// ask "does a trip let this caller read this car's history, and how much of
// it?".
//
// Implementations apply the live-share join themselves — the join IS the access
// check — so a participant whose grant was revoked yields no window and is
// denied with everybody else.
//
// NIL IS THE FAIL-CLOSED DEFAULT. A handler with no admitter wired behaves
// exactly as it did before MYR-602: owner or nobody. A deployment that forgot
// to wire trips under-serves rather than over-shares.
type TripDriveAdmitter interface {
	// TripDriveWindows returns every window on this vehicle that admits this
	// caller to its drives, or an empty slice for none. An empty slice is a
	// DENIAL and callers must treat it as one — never as "all of them".
	TripDriveWindows(ctx context.Context, userID, vehicleID string) ([]TripDriveWindow, error)

	// VehicleDrivesInTripWindows is §7.2 narrowed to those windows.
	//
	// A SEPARATE METHOD RATHER THAN A FILTER OVER THE OWNER'S PAGE, because
	// filtering after pagination is how a page of ten becomes a page of two
	// while eight matching drives sit behind the cursor. The narrowing has to
	// happen in the statement that applies the LIMIT, so it has to happen in
	// the store.
	//
	// Implementations MUST re-resolve the windows from userID rather than
	// accept them from the caller: a signature that took a window would let
	// somebody on trip A read trip B's drives by supplying B's dates.
	VehicleDrivesInTripWindows(
		ctx context.Context, userID, vehicleID string, cursor DriveListCursor, limit int,
	) (DriveListPage, error)
}

// tripDriveAdmission is the resolved answer for one request.
type tripDriveAdmission struct {
	// Windows is what the caller may read. Empty means a trip admits them to
	// nothing on this car.
	Windows []TripDriveWindow
}

// participant reports whether a trip admits this caller to ANY of the vehicle's
// history. It is what separates "outside your window" (404) from "you have no
// business here at all" (403).
func (a tripDriveAdmission) participant() bool { return len(a.Windows) > 0 }

// covers reports whether a specific drive falls inside any admitted window.
func (a tripDriveAdmission) covers(startedAt time.Time) bool {
	for _, w := range a.Windows {
		if w.Covers(startedAt) {
			return true
		}
	}
	return false
}

// resolveTripDriveAdmission asks the admitter, treating every failure as "no
// windows".
//
// FAILS CLOSED BY RETURNING NO WINDOWS RATHER THAN AN ERROR, the same posture
// the auth package's trip probe takes and for the same reason: this lookup runs
// on a path that has a correct answer without it (the owner check, or the
// existing 403). A database blip must narrow the response, never convert a
// request into a 500 — and never widen it. The failure is logged so it is
// visible as an outage rather than as a permissions mystery.
func resolveTripDriveAdmission(
	ctx context.Context, trips TripDriveAdmitter, logger *slog.Logger, surface, userID, vehicleID string,
) tripDriveAdmission {
	if trips == nil {
		return tripDriveAdmission{}
	}
	windows, err := trips.TripDriveWindows(ctx, userID, vehicleID)
	if err != nil {
		logger.Error(surface+": trip window lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return tripDriveAdmission{}
	}
	return tripDriveAdmission{Windows: windows}
}

// denyDriveOutsideTripWindow writes the 404 a participant gets for a drive
// their window does not cover.
//
// The message names no drive, no date and no vehicle beyond what the caller
// already put in the URL: the whole point of answering 404 rather than 403 is
// that the response says nothing about whether the drive is real.
func denyDriveOutsideTripWindow(w http.ResponseWriter, logger *slog.Logger, surface, driveID, userID string) {
	logger.Warn(surface+": drive outside the caller's trip windows",
		slog.String("drive_id", driveID),
		slog.String("user_id", userID),
	)
	wserrors.WriteErrorEnvelope(w, logger, http.StatusNotFound,
		wserrors.ErrCodeNotFound, "drive not found")
}

// parseDriveStartTime reads a Drive row's RFC 3339 `startTime`.
//
// A drive whose start time will not parse is admitted to NOBODY through a trip:
// the window test cannot be evaluated, and the fail-closed answer for an
// unevaluable access check is denial. The owner path never reaches here.
func parseDriveStartTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
