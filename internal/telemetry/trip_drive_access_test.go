package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The MYR-602 window gate on the drives surfaces, at the HANDLER.
//
// MYR-369 made drive history owner-only and that still stands for `viewer` and
// `ride_member`. A trip participant is the one exception, and these pin its two
// halves: the LIST is narrowed in the store rather than filtered here, and the
// SINGLE-DRIVE refusal is 404 rather than 403 — while the plain-viewer 403 is
// untouched, because it answers a different question.

const (
	tripDriveStranger = "usr_stranger"
	tripDriveOwner    = "usr_drive_owner"
)

// stubTripAdmitter scripts the window gate.
type stubTripAdmitter struct {
	windows []TripDriveWindow
	err     error

	page      DriveListPage
	pageErr   error
	pageCalls int
}

func (s *stubTripAdmitter) TripDriveWindows(context.Context, string, string) ([]TripDriveWindow, error) {
	return s.windows, s.err
}

func (s *stubTripAdmitter) VehicleDrivesInTripWindows(
	context.Context, string, string, DriveListCursor, int,
) (DriveListPage, error) {
	s.pageCalls++
	return s.page, s.pageErr
}

// TestVehicleDrivesAdmitsATripParticipantThroughTheNarrowedQuery.
//
// THE NARROWING HAPPENS IN THE STATEMENT THAT APPLIES THE LIMIT, not by
// filtering an owner's page — filtering after pagination is how a page of ten
// becomes a page of two while eight matching drives sit behind the cursor. This
// asserts the handler routes a participant to the windowed lister and never
// touches the owner's.
func TestVehicleDrivesAdmitsATripParticipantThroughTheNarrowedQuery(t *testing.T) {
	ownerLister := &stubDriveLister{page: DriveListPage{Items: fixtureDriveItems(fixtureSnapshotRowID, 5)}}
	admitter := &stubTripAdmitter{
		windows: []TripDriveWindow{{From: time.Now().Add(-48 * time.Hour), To: time.Now()}},
		page:    DriveListPage{Items: fixtureDriveItems(fixtureSnapshotRowID, 2)},
	}

	h := NewVehicleDrivesHandler(
		&stubTokenValidator{userID: tripDriveStranger},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(tripDriveOwner)},
		ownerLister,
		discardLogger(),
		WithDrivesTripAdmitter(admitter),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/drives", h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/vehicles/"+fixtureSnapshotRowID+"/drives", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	if admitter.pageCalls != 1 {
		t.Fatalf("the windowed lister ran %d times, want 1", admitter.pageCalls)
	}
	// THE OWNER'S LISTER MUST NOT HAVE RUN. If it had, the participant would
	// have been served the car's whole history and then — at best — filtered.
	if ownerLister.lastLim != 0 {
		t.Fatalf("the unnarrowed owner lister ran for a participant")
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("served %d drives, want the window's 2", len(body.Items))
	}
}

// TestVehicleDrivesStillRefusesACallerWithNoWindow pins that the pre-MYR-602
// behaviour is unchanged for everybody else. No windows — or a lookup failure —
// and the original 403 stands.
func TestVehicleDrivesStillRefusesACallerWithNoWindow(t *testing.T) {
	cases := []struct {
		name     string
		admitter *stubTripAdmitter
	}{
		{"no trip at all", &stubTripAdmitter{}},
		{"a lookup failure fails closed", &stubTripAdmitter{err: errors.New("connection reset")}},
		{"no admitter wired at all", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []VehicleDrivesOption{}
			if tc.admitter != nil {
				opts = append(opts, WithDrivesTripAdmitter(tc.admitter))
			}
			h := NewVehicleDrivesHandler(
				&stubTokenValidator{userID: tripDriveStranger},
				&stubVehicleSnapshotReader{row: fixtureSnapshotRow(tripDriveOwner)},
				&stubDriveLister{},
				discardLogger(),
				opts...,
			)
			mux := http.NewServeMux()
			mux.Handle("GET /api/vehicles/{vehicleId}/drives", h)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
				"/api/vehicles/"+fixtureSnapshotRowID+"/drives", nil)
			req.Header.Set("Authorization", "Bearer token")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want the unchanged 403. Body: %s", rec.Code, rec.Body.String())
			}
			var env wserrors.ErrorEnvelope
			if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != wserrors.ErrCodeVehicleNotOwned {
				t.Errorf("code = %q, want vehicle_not_owned", env.Error.Code)
			}
		})
	}
}

// TestVehicleDrivesLeavesTheOwnerPathUntouched. An owner must still read their
// whole history through the unnarrowed lister, with the trip probe never
// running — it is only reached AFTER the owner check has already failed, so the
// owner path costs nothing.
func TestVehicleDrivesLeavesTheOwnerPathUntouched(t *testing.T) {
	ownerLister := &stubDriveLister{page: DriveListPage{Items: fixtureDriveItems(fixtureSnapshotRowID, 5)}}
	admitter := &stubTripAdmitter{windows: []TripDriveWindow{{From: time.Now().Add(-time.Hour), To: time.Now()}}}

	h := NewVehicleDrivesHandler(
		&stubTokenValidator{userID: tripDriveOwner},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(tripDriveOwner)},
		ownerLister,
		discardLogger(),
		WithDrivesTripAdmitter(admitter),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/drives", h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/vehicles/"+fixtureSnapshotRowID+"/drives", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	if admitter.pageCalls != 0 {
		t.Fatalf("the windowed lister ran for an OWNER (%d calls)", admitter.pageCalls)
	}
	if ownerLister.lastLim == 0 {
		t.Fatal("the owner's own lister did not run")
	}
}

// TestTripDriveWindowCoversIsInclusiveAtBothEdges.
//
// A drive that began exactly at the closing instant is a drive of that trip;
// excluding it would lose it from the only list it belongs to. The asymmetry
// with the ACCESS predicate's exclusive upper bound is deliberate — that one is
// about a live socket at an instant.
func TestTripDriveWindowCoversIsInclusiveAtBothEdges(t *testing.T) {
	from := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	w := TripDriveWindow{From: from, To: to}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"a second before the open", from.Add(-time.Second), false},
		{"exactly at the open", from, true},
		{"inside", from.Add(24 * time.Hour), true},
		{"exactly at the close", to, true},
		{"a second after the close", to.Add(time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.Covers(tc.at); got != tc.want {
				t.Fatalf("Covers(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// TestUnparseableDriveStartTimeIsAdmittedToNobody.
//
// The window test cannot be evaluated against a start time that will not parse,
// and the fail-closed answer for an unevaluable access check is denial. The
// owner path never reaches this helper.
func TestUnparseableDriveStartTimeIsAdmittedToNobody(t *testing.T) {
	if _, ok := parseDriveStartTime("not an instant"); ok {
		t.Fatal("a malformed startTime parsed")
	}
	if _, ok := parseDriveStartTime(""); ok {
		t.Fatal("an empty startTime parsed")
	}
	if _, ok := parseDriveStartTime("2026-09-01T12:00:00Z"); !ok {
		t.Fatal("a well-formed RFC 3339 startTime failed to parse")
	}
}
