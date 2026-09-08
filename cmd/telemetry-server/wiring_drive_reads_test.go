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

// THE MYR-614 WIRING TESTS: the REAL adapters feeding the REAL handlers, for
// BOTH drive reads — §7.4's route (wiring_drive_reads.go's newDriveRouteHandler)
// and §7.3's detail (newDriveDetailHandler).
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
// driveRouteAdapter / driveDetailAdapter over a scripted store.DriveRecord,
// wrapped by the real handler constructors with the real trip admitter and role
// resolver attached. The only fake below the HTTP boundary is the single-row
// store read.

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

// stubTripWindows scripts the §7.3/§7.4 admission. An empty slice is a denial,
// per the TripDriveAdmitter contract.
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

// serveDriveRoute runs one §7.4 request through the production graph and
// returns the recorder plus everything the handler logged.
func serveDriveRoute(
	t *testing.T, caller string, records stubDriveRecords, trips telemetry.TripDriveAdmitter,
) (*httptest.ResponseRecorder, string) {
	t.Helper()

	logs, deps := wiringDeps(caller)
	// THE REAL ADAPTER. Everything this test is about lives in the line below.
	h := newDriveRouteHandler(deps, wiringVehicles{}, trips, &driveRouteAdapter{repo: records})

	return serveDriveRequest(t, h, "GET /api/drives/{driveId}/route",
		"/api/drives/"+wiringDriveID+"/route"), logs.String()
}

// serveDriveDetail is serveDriveRoute's §7.3 twin. THE TWO SURFACES ARE TESTED
// THE SAME WAY ON PURPOSE: they run one gate over one set of access facts since
// MYR-614, so any case that matters on one matters identically on the other —
// and the detail arm shipped with no wiring coverage of the window gate at all,
// which is the hole that let the route arm's version of this bug through.
func serveDriveDetail(
	t *testing.T, caller string, records stubDriveRecords, trips telemetry.TripDriveAdmitter,
) (*httptest.ResponseRecorder, string) {
	t.Helper()

	logs, deps := wiringDeps(caller)
	h := newDriveDetailHandler(deps, wiringVehicles{}, trips, &driveDetailAdapter{repo: records})

	return serveDriveRequest(t, h, "GET /api/drives/{driveId}",
		"/api/drives/"+wiringDriveID), logs.String()
}

func wiringDeps(caller string) (*bytes.Buffer, httpRouteDeps) {
	var logs bytes.Buffer
	return &logs, httpRouteDeps{
		authenticator: wiringAuth{userID: caller},
		logger:        slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func serveDriveRequest(t *testing.T, h http.Handler, pattern, target string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(pattern, h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// driveReadSurface is one of the two endpoints, so the cases that must answer
// identically on both can be written once and run twice.
type driveReadSurface struct {
	name  string
	serve func(*testing.T, string, stubDriveRecords, telemetry.TripDriveAdmitter) (*httptest.ResponseRecorder, string)
}

func driveReadSurfaces() []driveReadSurface {
	return []driveReadSurface{
		{"§7.4 route", serveDriveRoute},
		{"§7.3 detail", serveDriveDetail},
	}
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

// TestDriveReadsRefuseAParticipantOutsideTheirTripWindow pins the OTHER half,
// on BOTH surfaces. The fix must not turn the window into a formality: a
// participant whose windows do not cover this drive still gets 404 — never 403,
// which would confirm the car made a journey on a day they were not part of.
func TestDriveReadsRefuseAParticipantOutsideTheirTripWindow(t *testing.T) {
	for _, s := range driveReadSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			rec, _ := s.serve(t, wiringParticipantID,
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
		})
	}
}

// TestDriveReadsStillRefuseACallerWithNoTripAtAll pins the pre-MYR-602 answer
// for everybody else: 403, not 404. This is a different question — "may you
// read this car's history at all" — and MYR-614 must not have moved it.
func TestDriveReadsStillRefuseACallerWithNoTripAtAll(t *testing.T) {
	for _, s := range driveReadSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			rec, _ := s.serve(t, wiringParticipantID,
				stubDriveRecords{rec: wiringDriveRecord(wiringDriveStart)},
				stubTripWindows{})

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want the unchanged 403. Body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDriveReadsServeTheOwnerRegardlessOfTrips. The owner branch returns before
// the window check ever runs, which is why the owner never saw this bug and why
// nobody found it from the inside.
func TestDriveReadsServeTheOwnerRegardlessOfTrips(t *testing.T) {
	for _, s := range driveReadSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			// Deliberately with NO windows and NO start time: neither is
			// consulted on the owner path, and if either ever becomes
			// load-bearing there, this fails rather than quietly gating the
			// owner.
			rec, _ := s.serve(t, wiringOwnerID,
				stubDriveRecords{rec: wiringDriveRecord("")},
				stubTripWindows{})

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for the owner. Body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDriveReadsAdmitEveryStartTimeShapeTheListAdmits.
//
// §7.2 selects a participant's drives with `"startTime"::timestamptz BETWEEN …`
// and §7.3/§7.4 parse the same column in Go. rest-api.md §5.2.4 says those two
// admit ONE SET — so a row Postgres reads as inside the window must open here,
// whatever ISO shape it is stored in. A strict RFC 3339 parse (what shipped
// before this round) refused `2026-09-07 21:14:00+00` and `2026-09-07` while
// the cast admitted both: two parsers, two answers, a drive listed and then
// unopenable.
//
// Run through the REAL adapter on BOTH surfaces, because the parse the gate
// applies is reached from the store row, not from a hand-set field.
func TestDriveReadsAdmitEveryStartTimeShapeTheListAdmits(t *testing.T) {
	// Every value below is the same instant as wiringDriveStart, or a
	// midnight inside the same window.
	shapes := []struct {
		name      string
		startTime string
	}{
		{"RFC 3339", "2026-09-07T21:14:00Z"},
		{"fractional seconds", "2026-09-07T21:14:00.482Z"},
		{"offset without a colon", "2026-09-07T21:14:00+0000"},
		{"space separator, hours-only offset", "2026-09-07 21:14:00+00"},
		{"no offset at all", "2026-09-07 21:14:00"},
		{"date only", "2026-09-07"},
	}

	for _, s := range driveReadSurfaces() {
		for _, shape := range shapes {
			t.Run(s.name+"/"+shape.name, func(t *testing.T) {
				rec, _ := s.serve(t, wiringParticipantID,
					stubDriveRecords{rec: wiringDriveRecord(shape.startTime)},
					stubTripWindows{windows: wiringTripWindow()})

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d for startTime %q, want 200 — `::timestamptz` "+
						"admits this shape into the §7.2 list, so the single-drive gate must "+
						"admit it too. Body: %s", rec.Code, shape.startTime, rec.Body.String())
				}
			})
		}
	}
}

// TestDriveReadsLogAnUnreadableStartTimeAndStillAnswer404 is the second half of
// the repair, and the reason the bug lasted as long as it did.
//
// A drive row whose startTime will not parse is a SERVER DATA FAULT: the gate
// cannot evaluate its own question. THE CALLER IS STILL TOLD ONLY 404 — a
// distinct status would tell a participant that this drive exists on this car,
// which is exactly what the 404-not-403 rule withholds, and the condition is
// permanent, so a 5xx would also invite an SDK to retry it forever. The fault
// goes where it cannot leak: an ERROR line naming the drive.
func TestDriveReadsLogAnUnreadableStartTimeAndStillAnswer404(t *testing.T) {
	cases := []struct {
		name      string
		startTime string
	}{
		// THE EXACT MYR-614 SHAPE: the field the adapter never set.
		{"absent — the MYR-614 regression", ""},
		{"not an instant at all", "yesterday afternoon"},
	}

	for _, s := range driveReadSurfaces() {
		for _, tc := range cases {
			t.Run(s.name+"/"+tc.name, func(t *testing.T) {
				rec, logs := s.serve(t, wiringParticipantID,
					stubDriveRecords{rec: wiringDriveRecord(tc.startTime)},
					stubTripWindows{windows: wiringTripWindow()})

				if rec.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want the ordinary 404 — a status of its own would "+
						"tell a participant the drive exists. Body: %s", rec.Code, rec.Body.String())
				}
				// AND IT MUST BE THE SAME 404, byte for byte, as a drive
				// genuinely outside the window. A distinguishable body is
				// the same oracle as a distinguishable status.
				outside, _ := s.serve(t, wiringParticipantID,
					stubDriveRecords{rec: wiringDriveRecord(wiringDriveStart)},
					stubTripWindows{windows: wiringPastWindow()})
				if rec.Body.String() != outside.Body.String() {
					t.Errorf("the data-fault refusal is distinguishable from the window refusal:\n"+
						" fault:   %s\n outside: %s", rec.Body.String(), outside.Body.String())
				}
				// THE LOG IS THE POINT. A fault nobody can see is the failure
				// mode being repaired, so the drive must be named at Error
				// level — that line is the only place this condition surfaces.
				if !strings.Contains(logs, "level=ERROR") {
					t.Errorf("the fault was not logged at ERROR level. Logs:\n%s", logs)
				}
				if !strings.Contains(logs, wiringDriveID) {
					t.Errorf("the log does not name the faulty drive. Logs:\n%s", logs)
				}
			})
		}
	}
}

// TestDriveReadsPassAMissingDriveThroughAs404. The adapter wraps the store's
// error with %w and store.ErrDriveNotFound wraps sdk.ErrNotFound; if that chain
// is ever broken the handler reports 500 for an ordinary unknown id.
func TestDriveReadsPassAMissingDriveThroughAs404(t *testing.T) {
	for _, s := range driveReadSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			rec, _ := s.serve(t, wiringOwnerID,
				stubDriveRecords{err: store.ErrDriveNotFound},
				stubTripWindows{windows: wiringTripWindow()})

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for an unknown drive id. Body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDriveDetailWiringCarriesTheSameAccessFacts. §7.3 and §7.4 share one
// DriveAccessFacts producer since MYR-614, and this is what makes that shared
// producer worth having: the same participant, the same window, the same drive
// — both surfaces must admit them. A drive whose stats a participant can open
// but whose route they are refused is the state this pair forbids.
func TestDriveDetailWiringCarriesTheSameAccessFacts(t *testing.T) {
	rec, _ := serveDriveDetail(t, wiringParticipantID,
		stubDriveRecords{rec: wiringDriveRecord(wiringDriveStart)},
		stubTripWindows{windows: wiringTripWindow()})

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

// TestDriveReadProjectionsDropNoField is the guard that makes the fix hold for
// the NEXT field rather than only for this one.
//
// MYR-320 and MYR-614 are the same bug twice: a value present in the store row,
// carried by the migration, the writer, the SELECT, the scan and the response
// struct, and dropped by ONE struct literal in cmd/ that nobody's test crossed.
// MYR-320 answered it with a reflective totality gate over
// snapshotRowFromVehicle (adapters_vehicle_snapshot_test.go); these are the same
// gate over the two drive-read projections, using the SAME fillNonZero helper —
// a hand-written source fixture would have to be updated by the same person who
// forgot the field, which is no guard at all, and would fail spuriously the
// moment DriveRecord grows a column the fixture does not spell.
func TestDriveReadProjectionsDropNoField(t *testing.T) {
	src := fullyPopulatedDriveRecord(t)
	records := stubDriveRecords{rec: src}

	detail, err := (&driveDetailAdapter{repo: records}).GetDriveDetail(context.Background(), wiringDriveID)
	if err != nil {
		t.Fatalf("GetDriveDetail: %v", err)
	}
	route, err := (&driveRouteAdapter{repo: records}).GetDriveRoute(context.Background(), wiringDriveID)
	if err != nil {
		t.Fatalf("GetDriveRoute: %v", err)
	}

	for _, tc := range []struct {
		name       string
		projection any
		adapter    string
	}{
		{"DriveDetailData (§7.3)", detail, "driveDetailAdapter"},
		{"DriveRouteData (§7.4)", route, "driveRouteAdapter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNoZeroField(t, reflect.ValueOf(tc.projection), tc.name, tc.adapter)
		})
	}
}

// TestDriveAccessFactsIsTotalOverTheStoreRow guards the identity BOTH gates
// consume. Adding a field to DriveAccessFacts and forgetting to fill it here
// would reintroduce MYR-614 exactly — a field the handler reads and the adapter
// leaves at its zero value, invisible to every test that builds the struct by
// hand.
func TestDriveAccessFactsIsTotalOverTheStoreRow(t *testing.T) {
	// Named assertions first, so a failure says WHICH fact and WHY, and the
	// reflective sweep below catches the field nobody thought to name.
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

	assertNoZeroField(t, reflect.ValueOf(driveAccessFacts(fullyPopulatedDriveRecord(t))),
		"DriveAccessFacts", "driveAccessFacts")
}

// assertNoZeroField fails for every field of a projection left at its zero
// value after projecting a fully-populated source row. It RECURSES into
// embedded and nested structs rather than testing them whole: DriveAccessFacts
// sits embedded in both read models, and a projection that filled one of its
// three fields would leave the embedded value non-zero and pass vacuously —
// which is the exact failure mode this exists to catch, one level down.
func assertNoZeroField(t *testing.T, v reflect.Value, path, adapter string) {
	t.Helper()

	rt := v.Type()
	for i := range rt.NumField() {
		name := path + "." + rt.Field(i).Name
		field := v.Field(i)
		if field.Kind() == reflect.Struct && field.Type() != reflect.TypeOf(time.Time{}) {
			assertNoZeroField(t, field, name, adapter)
			continue
		}
		if !field.IsZero() {
			continue
		}
		t.Errorf("%s is the zero value after projecting a fully-populated store.DriveRecord — "+
			"%s (adapters_drive_reads.go) does not copy it, so the endpoint serves a "+
			"default for a column the DB holds. This is MYR-614 (and MYR-320) again", name, adapter)
	}
}

// fullyPopulatedDriveRecord returns a store.DriveRecord whose every field holds
// a non-zero value, built by reflection with the same helper the MYR-320 gate
// uses (adapters_vehicle_snapshot_test.go) so a newly added column is populated
// automatically rather than silently left at its zero value.
func fullyPopulatedDriveRecord(t *testing.T) store.DriveRecord {
	t.Helper()

	var rec store.DriveRecord
	rv := reflect.ValueOf(&rec).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		if err := fillNonZero(rv.Field(i), rt.Field(i).Name); err != nil {
			t.Fatalf("populate store.DriveRecord.%s: %v", rt.Field(i).Name, err)
		}
	}
	return rec
}
