package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// THE MYR-614 WIRING TEST: the REAL adapter feeding the REAL handler.
//
// The bug this file exists to prevent was not in the adapter and not in the
// handler — both were correct read on their own. It was in the SEAM. The
// handler consumed DriveRouteData.StartTime as the §7.4 trip-window input; the
// adapter built the struct and never set the field. Every existing test on
// either side supplied its own value for that field by hand, so the whole test
// suite passed while every trip participant got a 404 on every drive route and
// the iOS summary map rendered "routeless" for every shared viewer.
//
// A test that stubs the fetcher cannot see a seam like that, however many cases
// it enumerates. So these drive the production object graph: the real
// driveRouteAdapter over a scripted store.DriveRecord, wrapped by the real
// newDriveRouteHandler with the real trip admitter and role resolver attached.
// The only fake below the HTTP boundary is the single-row store read.

const (
	wiringDriveID   = "clmno9876543210zyxw0614"
	wiringVehicleID = "cmphman6p000fkz04rq3adktk"
	wiringOwnerID   = "usr_owner_614"
	// wiringParticipantID is a NON-owner: no share of any shape admits them
	// to the drives surfaces (MYR-369). A trip window is their only way in.
	wiringParticipantID = "usr_participant_614"
)

// wiringDriveStart is the instant the scripted drive began — inside
// wiringTripWindow below, outside wiringPastWindow.
const wiringDriveStart = "2026-09-07T21:14:00Z"

// stubDriveRecords is the one fake: the store's single-drive read.
type stubDriveRecords struct {
	rec store.DriveRecord
	err error
}

func (s stubDriveRecords) GetByID(_ context.Context, _ string) (store.DriveRecord, error) {
	if s.err != nil {
		return store.DriveRecord{}, s.err
	}
	return s.rec, nil
}

// wiringDriveRecord is the row the store hands the adapter, shaped like the
// production rows behind the client report: a real start instant and a
// populated (decrypted) polyline.
func wiringDriveRecord(startTime string) store.DriveRecord {
	return store.DriveRecord{
		ID:        wiringDriveID,
		VehicleID: wiringVehicleID,
		Date:      "2026-09-07",
		StartTime: startTime,
		EndTime:   "2026-09-07T21:48:00Z",
		RoutePoints: json.RawMessage(
			`[{"lat":32.98246,"lng":-96.90867,"speed":0,"heading":357,"timestamp":"2026-09-07T21:14:00Z"}]`,
		),
	}
}

// stubTripWindows scripts the §7.4 admission. An empty slice is a denial, per
// the TripDriveAdmitter contract.
type stubTripWindows struct {
	windows []telemetry.TripDriveWindow
}

func (s stubTripWindows) TripDriveWindows(_ context.Context, _, _ string) ([]telemetry.TripDriveWindow, error) {
	return s.windows, nil
}

func (s stubTripWindows) VehicleDrivesInTripWindows(
	_ context.Context, _, _ string, _ telemetry.DriveListCursor, _ int,
) (telemetry.DriveListPage, error) {
	return telemetry.DriveListPage{}, nil
}

// wiringTripWindow COVERS the scripted drive — the shape of the window in the
// client report (2026-09-03T22:00Z → 2026-09-10T06:59Z).
func wiringTripWindow() []telemetry.TripDriveWindow {
	return []telemetry.TripDriveWindow{{
		From: time.Date(2026, 9, 3, 22, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 9, 10, 6, 59, 0, 0, time.UTC),
	}}
}

// wiringPastWindow is a real window that does NOT cover the scripted drive.
func wiringPastWindow() []telemetry.TripDriveWindow {
	return []telemetry.TripDriveWindow{{
		From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
	}}
}

// wiringAuth answers as one caller, and resolves every role as owner — the
// mask layer's role, not the gate's. The gate's answer comes from the vehicle
// row's UserID and the trip windows, which is the point.
type wiringAuth struct{ userID string }

func (a wiringAuth) ValidateToken(_ context.Context, token string) (string, error) {
	if token == "" {
		return "", auth.ErrInvalidToken
	}
	return a.userID, nil
}

func (a wiringAuth) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	return []string{wiringVehicleID}, nil
}

func (a wiringAuth) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	return auth.RoleOwner, nil
}

// wiringVehicles serves the drive's vehicle row, owned by wiringOwnerID.
type wiringVehicles struct{}

func (wiringVehicles) GetByID(_ context.Context, _ string) (telemetry.VehicleSnapshotRow, error) {
	return telemetry.VehicleSnapshotRow{ID: wiringVehicleID, UserID: wiringOwnerID}, nil
}

// serveDriveRoute runs one request through the production graph and returns
// the recorder plus everything the handler logged.
func serveDriveRoute(
	t *testing.T, caller string, records stubDriveRecords, trips telemetry.TripDriveAdmitter,
) (*httptest.ResponseRecorder, string) {
	t.Helper()

	var logs bytes.Buffer
	deps := httpRouteDeps{
		authenticator: wiringAuth{userID: caller},
		logger:        slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	// THE REAL ADAPTER. Everything this test is about lives in the line below.
	h := newDriveRouteHandler(deps, wiringVehicles{}, trips, &driveRouteAdapter{repo: records})

	mux := http.NewServeMux()
	mux.Handle("GET /api/drives/{driveId}/route", h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/drives/"+wiringDriveID+"/route", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec, logs.String()
}

// TestDriveRouteWiringAdmitsAParticipantInsideTheirTripWindow is the
// regression. Before MYR-614 this returned 404 with an empty polyline for
// every drive, because the adapter's DriveRouteData carried no StartTime and
// the window test therefore could not be evaluated.
func TestDriveRouteWiringAdmitsAParticipantInsideTheirTripWindow(t *testing.T) {
	rec, _ := serveDriveRoute(t, wiringParticipantID,
		stubDriveRecords{rec: wiringDriveRecord(wiringDriveStart)},
		stubTripWindows{windows: wiringTripWindow()})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a participant inside their window may read the route. Body: %s",
			rec.Code, rec.Body.String())
	}

	var resp struct {
		DriveID     string            `json:"driveId"`
		RoutePoints []json.RawMessage `json:"routePoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DriveID != wiringDriveID {
		t.Errorf("driveId = %q, want %q", resp.DriveID, wiringDriveID)
	}
	// THE POLYLINE ITSELF, not just a 200. The client-visible symptom was an
	// empty route, so an empty route must fail this test even if the status
	// somehow passes.
	if len(resp.RoutePoints) == 0 {
		t.Fatal("routePoints came back empty — the participant was admitted but got no polyline")
	}
}

// TestDriveRouteWiringRefusesAParticipantOutsideTheirTripWindow pins the OTHER
// half. The fix must not turn the window into a formality: a participant whose
// windows do not cover this drive still gets 404 — never 403, which would
// confirm the car made a journey on a day they were not part of.
func TestDriveRouteWiringRefusesAParticipantOutsideTheirTripWindow(t *testing.T) {
	rec, _ := serveDriveRoute(t, wiringParticipantID,
		stubDriveRecords{rec: wiringDriveRecord(wiringDriveStart)},
		stubTripWindows{windows: wiringPastWindow()})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a drive outside every window. Body: %s",
			rec.Code, rec.Body.String())
	}
	var env wserrors.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != wserrors.ErrCodeNotFound {
		t.Errorf("code = %q, want not_found", env.Error.Code)
	}
	// The refusal must say nothing about the drive it is refusing.
	if strings.Contains(strings.ToLower(env.Error.Message), "trip") ||
		strings.Contains(env.Error.Message, wiringVehicleID) {
		t.Errorf("the 404 message leaks why it refused: %q", env.Error.Message)
	}
}

// TestDriveRouteWiringStillRefusesACallerWithNoTripAtAll pins the pre-MYR-602
// answer for everybody else: 403, not 404. This is a different question —
// "may you read this car's history at all" — and MYR-614 must not have moved it.
func TestDriveRouteWiringStillRefusesACallerWithNoTripAtAll(t *testing.T) {
	rec, _ := serveDriveRoute(t, wiringParticipantID,
		stubDriveRecords{rec: wiringDriveRecord(wiringDriveStart)},
		stubTripWindows{})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the unchanged 403. Body: %s", rec.Code, rec.Body.String())
	}
}

// TestDriveRouteWiringServesTheOwnerRegardlessOfTrips. The owner branch returns
// before the window check ever runs, which is why the owner never saw this bug
// and why nobody found it from the inside.
func TestDriveRouteWiringServesTheOwnerRegardlessOfTrips(t *testing.T) {
	// Deliberately with NO windows and NO start time: neither is consulted on
	// the owner path, and if either ever becomes load-bearing there, this
	// fails rather than quietly gating the owner.
	rec, _ := serveDriveRoute(t, wiringOwnerID,
		stubDriveRecords{rec: wiringDriveRecord("")},
		stubTripWindows{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the owner. Body: %s", rec.Code, rec.Body.String())
	}
}

// TestDriveRouteWiringReportsAnUnreadableStartTimeAsAFault is the second half
// of the repair, and the reason the bug lasted as long as it did.
//
// A drive row whose startTime will not parse is a SERVER DATA FAULT: the gate
// cannot evaluate its own question. Answering it with the window refusal's 404
// makes a broken server indistinguishable from a working one — which is exactly
// what happened. It is now 500 plus an Error log, so the next adapter that
// forgets a field shows up in the error rate on the day it ships.
//
// The request is still REFUSED either way. Only the honesty changed.
func TestDriveRouteWiringReportsAnUnreadableStartTimeAsAFault(t *testing.T) {
	cases := []struct {
		name      string
		startTime string
	}{
		// THE EXACT MYR-614 SHAPE: the field the adapter never set.
		{"absent — the MYR-614 regression", ""},
		{"not an instant at all", "yesterday afternoon"},
		{"a date with no time", "2026-09-07"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, logs := serveDriveRoute(t, wiringParticipantID,
				stubDriveRecords{rec: wiringDriveRecord(tc.startTime)},
				stubTripWindows{windows: wiringTripWindow()})

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 — an unevaluable window is a data fault, "+
					"and reporting it as 404 is what hid MYR-614. Body: %s",
					rec.Code, rec.Body.String())
			}
			var env wserrors.ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != wserrors.ErrCodeInternalError {
				t.Errorf("code = %q, want internal_error", env.Error.Code)
			}
			// THE LOG IS THE POINT. A fault nobody can see is the failure mode
			// being repaired, so the drive must be named at Error level.
			if !strings.Contains(logs, "level=ERROR") {
				t.Errorf("the fault was not logged at ERROR level. Logs:\n%s", logs)
			}
			if !strings.Contains(logs, wiringDriveID) {
				t.Errorf("the log does not name the faulty drive. Logs:\n%s", logs)
			}
		})
	}
}

// TestDriveRouteWiringPassesAMissingDriveThroughAs404. The adapter wraps the
// store's error with %w and store.ErrDriveNotFound wraps sdk.ErrNotFound; if
// that chain is ever broken the handler reports 500 for an ordinary unknown id.
func TestDriveRouteWiringPassesAMissingDriveThroughAs404(t *testing.T) {
	rec, _ := serveDriveRoute(t, wiringOwnerID,
		stubDriveRecords{err: store.ErrDriveNotFound},
		stubTripWindows{windows: wiringTripWindow()})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown drive id. Body: %s", rec.Code, rec.Body.String())
	}
}

// TestDriveDetailWiringCarriesTheSameAccessFacts. §7.3 and §7.4 share one
// DriveAccessFacts producer since MYR-614, and this is what makes that shared
// producer worth having: the same participant, the same window, the same drive
// — both surfaces must admit them. A drive whose stats a participant can open
// but whose route they are refused is the state this pair forbids.
func TestDriveDetailWiringCarriesTheSameAccessFacts(t *testing.T) {
	records := stubDriveRecords{rec: wiringDriveRecord(wiringDriveStart)}
	trips := stubTripWindows{windows: wiringTripWindow()}

	var logs bytes.Buffer
	deps := httpRouteDeps{
		authenticator: wiringAuth{userID: wiringParticipantID},
		logger:        slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	h := newDriveDetailHandler(deps, wiringVehicles{}, trips, &driveDetailAdapter{repo: records})

	mux := http.NewServeMux()
	mux.Handle("GET /api/drives/{driveId}", h)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/drives/"+wiringDriveID, nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — §7.3 must admit whoever §7.4 admits. Body: %s",
			rec.Code, rec.Body.String())
	}
	// The embedded DriveID must still reach the `id` wire field: the embedding
	// renamed the struct field, and a projection that dropped it would emit an
	// empty id rather than fail to compile.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["id"] != wiringDriveID {
		t.Errorf("id = %v, want %q", body["id"], wiringDriveID)
	}
	if body["startTime"] != wiringDriveStart {
		t.Errorf("startTime = %v, want %q", body["startTime"], wiringDriveStart)
	}
}

// TestDriveAccessFactsIsTotalOverTheStoreRow is the guard that makes the fix
// hold for the NEXT field rather than only for this one.
//
// driveAccessFacts is the single producer of the identity both drive gates
// consume. Adding a field to DriveAccessFacts and forgetting to fill it here
// would reintroduce MYR-614 exactly — a field the handler reads and the adapter
// leaves at its zero value, invisible to every test that builds the struct by
// hand. So: project a row whose every access field is distinctly non-zero and
// assert the result has no zero values left.
func TestDriveAccessFactsIsTotalOverTheStoreRow(t *testing.T) {
	facts := driveAccessFacts(wiringDriveRecord(wiringDriveStart))

	if facts.DriveID != wiringDriveID {
		t.Errorf("DriveID = %q, want %q", facts.DriveID, wiringDriveID)
	}
	if facts.VehicleID != wiringVehicleID {
		t.Errorf("VehicleID = %q, want %q", facts.VehicleID, wiringVehicleID)
	}
	// THE FIELD MYR-614 WAS ABOUT.
	if facts.StartTime != wiringDriveStart {
		t.Errorf("StartTime = %q, want %q — this is the MYR-614 field", facts.StartTime, wiringDriveStart)
	}
	// REFLECTIVE, NOT A HAND-WRITTEN LIST. A list would have to be updated by
	// the same person who forgot the field, which is no guard at all. Every
	// field of DriveAccessFacts is filled from the row above, so any field
	// still at its zero value is one the producer does not set.
	v := reflect.ValueOf(facts)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("driveAccessFacts left %s at its zero value — the handler reads it "+
				"and the adapter does not fill it, which is MYR-614 again",
				v.Type().Field(i).Name)
		}
	}
}
