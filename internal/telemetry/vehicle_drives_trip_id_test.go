package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MYR-608 — `DriveSummary.tripId` ON THE WIRE.
//
// The store decides WHICH trip a drive belongs to (and it decides it three
// different ways for three different callers — see
// internal/store/trip_drive_totals_test.go). What these tests pin is the part
// the database cannot: that the key survives the mask, that it is ALWAYS
// PRESENT, that "no trip" is `null` rather than an absent key, and that the
// handler forwards the CALLER — because the trip ids on an owner's list are
// scoped to that owner's own windows and a handler that stopped passing the
// user id would blank the field for everybody without failing anything.

// TestDriveSummaryCarriesTripID covers the two spellings on one page.
func TestDriveSummaryCarriesTripID(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-1"
		tripID    = "cltrip0001"
	)

	items := fixtureDriveItems(vehicleID, 2)
	items[0].TripID = strPtr(tripID)
	// items[1] deliberately has no trip: most drives fall in no window, and
	// that is the ordinary row rather than the exception.

	drives := &stubDriveLister{page: DriveListPage{Items: items}}
	rows := driveRowsFor(t, drives, vehicleID, userID)

	if len(rows) != 2 {
		t.Fatalf("items = %d, want 2", len(rows))
	}

	if got := rows[0]["tripId"]; got != tripID {
		t.Errorf("items[0].tripId = %v, want %q", got, tripID)
	}

	// ALWAYS PRESENT, NULL WHEN THERE IS NO TRIP — deliberately NOT the
	// omit-when-empty convention the four location labels on this same shape
	// follow. Those are absent because the server has nothing to say YET (the
	// geocode may still arrive); this is a DECIDED answer, and a missing key
	// would be indistinguishable from a server that does not send the field at
	// all. Grouping drives under trips is exactly the feature that must not
	// silently fall back to ungrouped.
	value, present := rows[1]["tripId"]
	if !present {
		t.Fatalf("items[1] has no tripId key at all; it must be present and null: %v", rows[1])
	}
	if value != nil {
		t.Errorf("items[1].tripId = %v, want null", value)
	}
}

// TestDriveListForwardsTheCallerToTheLister is the plumbing assertion, and it
// exists because the failure it guards is invisible.
//
// `tripId` on an owner's list is resolved against THAT OWNER's windows — a car
// can change hands, and a previous owner's trip must not be named to the new
// one. A handler that dropped the caller on the floor would still return 200,
// still return every drive, and simply report `null` for every trip. Nothing
// but this test would notice.
func TestDriveListForwardsTheCallerToTheLister(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-owner-608"
	)

	drives := &stubDriveLister{page: DriveListPage{Items: fixtureDriveItems(vehicleID, 1)}}
	driveRowsFor(t, drives, vehicleID, userID)

	if drives.lastViewer != userID {
		t.Fatalf("the lister was called with viewerUserID %q, want the authenticated caller %q", drives.lastViewer, userID)
	}
}

// TestTripDrivesPageCarriesTripID pins the same key on §7.30.7, which runs a
// DIFFERENT projection path (the `trip_participant` mask, no role resolver).
//
// A field added to §7.2's map and forgotten in the mask's allow-list would be
// stripped here and nowhere else, because this is the one drives surface whose
// role is a constant.
func TestTripDrivesPageCarriesTripID(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		tripID    = "cltrip0001"
	)

	items := fixtureDriveItems(vehicleID, 1)
	items[0].TripID = strPtr(tripID)

	page := tripDrivesPage(DriveListPage{Items: items})
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	if got := page.Items[0]["tripId"]; got != tripID {
		t.Fatalf("tripId = %v, want %q — the trip_participant mask must not strip it", got, tripID)
	}
}

// driveRowsFor runs one §7.2 request against a stub lister and returns the
// decoded item maps.
func driveRowsFor(t *testing.T, drives DriveLister, vehicleID, userID string) []map[string]any {
	t.Helper()

	h := NewVehicleDrivesHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
		drives,
		discardLogger(),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/drives", h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/vehicles/"+vehicleID+"/drives", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Items
}
