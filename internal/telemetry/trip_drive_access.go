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
	writeDriveNotFound(w, logger)
}

// denyDriveWithUnreadableStartTime answers a participant read of a drive whose
// stored `startTime` will not parse.
//
// THE SAME 404 AS "OUTSIDE YOUR WINDOW", AND THAT IS THE POINT (MYR-614).
//
// A participant must never learn from a STATUS whether a drive exists. That is
// the entire reason §7.3/§7.4 answer this role 404 rather than 403, and a
// status that appears only for drives the server actually holds is that oracle
// rebuilt one condition later: a caller who could tell "unreadable row" from
// "not yours" would have a probe for the car's history on days they were not
// part of. The condition is also PERMANENT — the row does not repair itself —
// so a 5xx would additionally invite every retry-on-5xx client to re-ask
// forever where a terminal 404 stops.
//
// A DATA FAULT IS LOGGED, NEVER TOLD TO A PARTICIPANT. The fault is real and
// MYR-614 is what it costs when nobody sees it: §7.4's adapter shipped without
// a `startTime`, every participant's every route parsed as "" and was refused,
// and the response was indistinguishable from the feature working correctly
// until a client reported it. So the honesty goes where it cannot leak — an
// ERROR line naming the drive, the vehicle and the value that would not parse,
// which is the line an operator greps and an alert can match on. The wire
// answer stays the refusal it always was.
//
// STILL ADMITTED TO NOBODY, either way. Only where the fault is reported moved.
func denyDriveWithUnreadableStartTime(
	w http.ResponseWriter, logger *slog.Logger, surface string, facts DriveAccessFacts, userID string,
) {
	logger.Error(surface+": drive startTime is missing or unparseable — cannot evaluate the trip window",
		slog.String("drive_id", facts.DriveID),
		slog.String("vehicle_id", facts.VehicleID),
		slog.String("user_id", userID),
		slog.String("start_time", facts.StartTime),
	)
	writeDriveNotFound(w, logger)
}

// writeDriveNotFound is the ONE writer for the participant 404, shared by both
// refusals above so they cannot drift into distinguishable answers. A byte for
// byte identical body is the whole mechanism: the two conditions must be one
// answer on the wire.
func writeDriveNotFound(w http.ResponseWriter, logger *slog.Logger) {
	wserrors.WriteErrorEnvelope(w, logger, http.StatusNotFound,
		wserrors.ErrCodeNotFound, "drive not found")
}
