package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The MYR-602 TRIP-DRIVES gate and list.
//
// A trip is the FIRST thing that lets a non-owner read a vehicle's drives.
// MYR-369 made drive history owner-only and that still stands for `viewer` and
// `ride_member`; a trip participant is admitted, and only to the drives inside
// a window they were part of. The bound is enforced here and at the handler,
// never by the field mask — a mask can hide fields and cannot hide a row.

// TripDriveWindows returns every window on ONE vehicle that admits this caller
// to that vehicle's drives, newest window first.
//
// ACTIVE **OR ENDED**, which is wider than the live-access predicate, and
// deliberately so. Live location is a window-scoped grant that ends with the
// window; the window's DRIVES are the record of a journey the person was
// actually part of, and having the list go dark at the moment the trip ends
// would delete the feature exactly when it becomes worth reading. SCHEDULED
// windows are excluded — a window that has not opened contains no drives, and
// admitting one would let an owner grant read access to the PAST by scheduling
// a trip for next week.
//
// An empty result is a DENIAL, and the caller must treat it as one: no window
// means no drive of this vehicle is readable, not "all of them".
func (r *TripRepo) TripDriveWindows(ctx context.Context, userID, vehicleID string) ([]TripDrivesWindow, error) {
	const op = "trip.drive_windows"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	rows, err := r.pool.Query(ctx, queryTripWindowsForUserVehicle, userID, vehicleID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.TripDriveWindows(vehicle=%s): %w", vehicleID, err)
	}
	defer rows.Close()

	out := make([]TripDrivesWindow, 0, 2)
	for rows.Next() {
		w := TripDrivesWindow{VehicleID: vehicleID}
		if err := rows.Scan(&w.From, &w.To); err != nil {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.TripDriveWindows(vehicle=%s): scan: %w", vehicleID, err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.TripDriveWindows(vehicle=%s): rows: %w", vehicleID, err)
	}
	return out, nil
}

// CoversDrive answers the §7.3 / §7.4 single-drive gate: may this caller read a
// drive of this vehicle that started at this instant?
//
// Written as a fold over TripDriveWindows rather than as its own EXISTS
// statement, so the set of windows that admits a LIST and the set that admits a
// DETAIL is provably the same set. Two statements would be two chances to write
// the predicate differently, and the difference would be a participant who can
// see a drive in the list and gets 404 opening it — or, in the direction that
// matters, the reverse.
//
// The bound is INCLUSIVE at both ends, matching Trip.Window(): a drive that
// began exactly at the closing instant is a drive of this trip.
func (r *TripRepo) CoversDrive(ctx context.Context, userID, vehicleID string, startedAt time.Time) (bool, error) {
	windows, err := r.TripDriveWindows(ctx, userID, vehicleID)
	if err != nil {
		return false, err
	}
	for _, w := range windows {
		if !startedAt.Before(w.From) && !startedAt.After(w.To) {
			return true, nil
		}
	}
	return false, nil
}

// TripDrivesForUser lists the drives of ONE trip's window, newest first, with
// the §7.2 cursor.
//
// THE WINDOW IS RE-READ FROM THE TRIP, never taken from the caller. The
// handler passes a trip id and a user id; this resolves the trip (which
// re-checks the caller's relationship to it) and derives the window from the
// row. A signature that accepted a window would let a caller who is on trip A
// read trip B's drives by supplying B's dates.
func (r *TripRepo) TripDrivesForUser(
	ctx context.Context, tripID, userID string, cursor DriveListCursor, limit int,
) (DriveListPage, error) {
	const op = "trip.drives"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	// GetForUser is the gate: ErrTripNotFound for a stranger, for a departed
	// participant and for an unknown id alike.
	trip, err := r.GetForUser(ctx, tripID, userID)
	if err != nil {
		return DriveListPage{}, err
	}
	w := trip.Window()

	probe := limit + 1
	rows, err := func() (pgx.Rows, error) {
		if cursor.StartTime == "" || cursor.ID == "" {
			return r.pool.Query(ctx, queryTripDrivesWindow, w.VehicleID, w.From, w.To, probe)
		}
		return r.pool.Query(ctx, queryTripDrivesWindowCursor,
			w.VehicleID, w.From, w.To, cursor.StartTime, cursor.ID, probe)
	}()
	if err != nil {
		r.metrics.IncQueryError(op)
		return DriveListPage{}, fmt.Errorf("TripRepo.TripDrivesForUser(%s): %w", tripID, err)
	}
	defer rows.Close()

	out := make([]DriveSummaryRow, 0, probe)
	for rows.Next() {
		// The SAME scanner the §7.2 list uses, over the same projection, so a
		// drive cannot render one way in a trip and another way in the car's
		// own history. It lives on DriveRepo because that is where the drive
		// label decryption lives; a trip-local copy would be a second place to
		// get the fail-soft label rules right.
		d, scanErr := scanDriveSummary(rows, r.encryptor, r.logger, r.metrics)
		if scanErr != nil {
			r.metrics.IncQueryError(op)
			return DriveListPage{}, fmt.Errorf("TripRepo.TripDrivesForUser(%s): %w", tripID, scanErr)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return DriveListPage{}, fmt.Errorf("TripRepo.TripDrivesForUser(%s): rows: %w", tripID, err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return DriveListPage{Items: out, HasMore: hasMore}, nil
}
