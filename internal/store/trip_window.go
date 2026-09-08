package store

import "time"

// MYR-608 — THE DRIVE-SELECTION WINDOW, split out of trip_types.go.
//
// ⚠ WHY IT IS ITS OWN FILE. trip_types.go crossed the 300-line cap (CLAUDE.md
// "File Rules") when `TripDrivesWindow` grew its `TripID`, and that file claims
// no exemption — trip_queries.go is the one file in the trips code that does,
// and its exemption is argued from a property of the STATEMENT SET which does
// not transfer to a list of struct definitions.
//
// THIS IS THE RIGHT SEAM rather than the cheapest one. `TripDrivesWindow` and
// `Trip.Window()` are one idea — "which drives does this trip's window select,
// and how is that pair carried so it cannot be mismatched" — and they are the
// only two declarations in the trips domain types that answer a question about
// DRIVES rather than about the trip row. Splitting anywhere else (the errors,
// the four row structs) would have separated declarations that are read
// together. The `TripDriveTotals` numbers deliberately do NOT live here: they
// are fields on `TripView` beside the roster and the vehicle, decorating a trip
// rather than describing a window.
//
// The bound itself is argued in trip_queries.go beside the SQL that applies it;
// this file carries the Go shape of it.

// TripDrivesWindow is the (vehicle, window) pair a trip's drive list resolves
// to. Carried as a type so the drive queries cannot be handed a window from one
// trip and a vehicle from another.
type TripDrivesWindow struct {
	VehicleID string
	From      time.Time
	To        time.Time

	// TripID is the window's own trip (MYR-608). It is carried WITH the bounds
	// rather than resolved beside them so `DriveSummary.tripId` can be read out
	// of the very window set that admitted a row — a window without its id
	// would force a second resolution, and a second resolution is a second
	// chance to name a trip that admitted nothing.
	TripID string
}

// Window returns the drive-selection window for a trip: drives whose startedAt
// falls in [startsAt, effectiveEnd]. WINDOW-BASED, NOT TAG-BASED — no write
// happens at drive time, so a trip created after the fact picks up the legs
// already driven, and extending a window backfills automatically.
//
// The upper bound is INCLUSIVE here where the access predicate's is exclusive,
// and the asymmetry is deliberate: access is about a live socket at an instant,
// whereas a drive that began exactly at the closing instant is a drive of this
// trip and excluding it would lose it from the only list it belongs to.
func (t Trip) Window() TripDrivesWindow {
	return TripDrivesWindow{
		VehicleID: t.VehicleID,
		From:      t.StartsAt,
		To:        t.EffectiveEnd(),
		TripID:    t.ID,
	}
}
