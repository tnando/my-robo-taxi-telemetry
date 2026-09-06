package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// The MYR-602 catalog leg: the rows a trip ADDS, and the `activeTripId` it
// stamps on rows the earlier legs produced.

// fakeTripVehicleLister scripts both halves of the merge independently, because
// they fail independently by design.
type fakeTripVehicleLister struct {
	rows    []TripVehicleRow
	rowsErr error

	active    map[string]string
	activeErr error
}

func (f *fakeTripVehicleLister) ListTripVehiclesByUser(context.Context, string) ([]TripVehicleRow, error) {
	return f.rows, f.rowsErr
}

func (f *fakeTripVehicleLister) ActiveTripIDsByUser(context.Context, string) (map[string]string, error) {
	return f.active, f.activeErr
}

// catalogItems runs the list handler and returns the decoded rows by vehicleId.
func catalogItems(t *testing.T, opts ...VehiclesListOption) map[string]map[string]any {
	t.Helper()

	h := NewVehiclesListHandler(
		&stubTokenValidator{userID: tripTestOwner},
		&stubVehicleLister{rows: []VehicleCatalogRow{{
			ID: tripTestVehicle, VIN: "7SAYGDET7TA613795", Name: "Roadie",
			Model: "Model Y", Year: 2024, Color: "UltraRed", Status: "parked",
		}}},
		discardLogger(),
		opts...,
	)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make(map[string]map[string]any, len(body.Items))
	for _, item := range body.Items {
		id, _ := item["vehicleId"].(string)
		out[id] = item
	}
	return out
}

// TestActiveTripIDIsStampedOnTheOwnersOwnRow.
//
// An owner's cars arrive through the FIRST merge leg, not the trip leg — a
// trip adds no row for a car you already own. The `activeTripId` still has to
// reach them, because the owner's own trip card is the surface the field exists
// for on that side.
func TestActiveTripIDIsStampedOnTheOwnersOwnRow(t *testing.T) {
	items := catalogItems(t, WithTripVehicles(&fakeTripVehicleLister{
		active: map[string]string{tripTestVehicle: tripTestID},
	}))

	row, ok := items[tripTestVehicle]
	if !ok {
		t.Fatalf("the owner's own car is missing: %v", items)
	}
	if row["activeTripId"] != tripTestID {
		t.Fatalf("activeTripId = %v, want %q", row["activeTripId"], tripTestID)
	}
}

// TestActiveTripIDIsAbsentNotNullWhenThereIsNoWindow.
//
// ABSENCE is the spelling, not null. The contract marks the field optional, and
// absence is what a pre-v0.41.0 server produces too — so a client that must
// handle absence anyway handles both, and there is no third state to reason
// about.
func TestActiveTripIDIsAbsentNotNullWhenThereIsNoWindow(t *testing.T) {
	items := catalogItems(t, WithTripVehicles(&fakeTripVehicleLister{}))

	row := items[tripTestVehicle]
	if _, present := row["activeTripId"]; present {
		t.Fatalf("activeTripId is present with no open window: %v", row["activeTripId"])
	}
}

// TestTripMergeFailuresDegradeIndependently.
//
// The two halves fail apart on purpose: an owner can lose the added rows and
// keep `activeTripId` on their own car, which is the row that matters most to
// them. And neither failure may 500 the catalog — the degraded response is a
// strictly SMALLER set, never a wider one, so an owner's own garage never goes
// down because a trip join hiccuped.
func TestTripMergeFailuresDegradeIndependently(t *testing.T) {
	t.Run("the row merge fails and the stamp survives", func(t *testing.T) {
		items := catalogItems(t, WithTripVehicles(&fakeTripVehicleLister{
			rowsErr: errors.New("connection reset"),
			active:  map[string]string{tripTestVehicle: tripTestID},
		}))
		if items[tripTestVehicle]["activeTripId"] != tripTestID {
			t.Fatalf("the stamp was lost with the row merge: %v", items[tripTestVehicle])
		}
	})

	t.Run("the stamp fails and the rows survive", func(t *testing.T) {
		items := catalogItems(t, WithTripVehicles(&fakeTripVehicleLister{
			activeErr: errors.New("connection reset"),
			rows: []TripVehicleRow{{
				VehicleCatalogRow: VehicleCatalogRow{
					ID: "cveh_trip_only", VIN: "7SAYGDET7TA999999", Name: "Nabil's car",
					Model: "Model S", Year: 2023, Color: "black", Status: "parked",
				},
				TripID: tripTestID,
			}},
		}))
		if _, ok := items["cveh_trip_only"]; !ok {
			t.Fatalf("the trip row was lost with the stamp: %v", items)
		}
		if _, present := items["cveh_trip_only"]["activeTripId"]; present {
			t.Errorf("a stamp appeared despite the lookup failing")
		}
	})
}

// TestTripRowsCarryTheWireViewerRoleAndTheElevatedFields.
//
// The SPLIT is the whole point. The MASK role is `trip_participant` — the
// viewer field set PLUS the location group — because that is what the caller is
// entitled to inside a window. The WIRE role is `viewer`, because
// VehicleSummary.role is a CLOSED enum every shipped client decodes, and
// emitting an internal RBAC name would fail the whole row rather than degrade.
func TestTripRowsCarryTheWireViewerRoleAndTheElevatedFields(t *testing.T) {
	lat, lng := 32.7767, -96.797
	items := catalogItems(t, WithTripVehicles(&fakeTripVehicleLister{
		active: map[string]string{"cveh_trip_only": tripTestID},
		rows: []TripVehicleRow{{
			VehicleCatalogRow: VehicleCatalogRow{
				ID: "cveh_trip_only", VIN: "7SAYGDET7TA999999", Name: "Nabil's car",
				Model: "Model S", Year: 2023, Color: "black", Status: "driving",
				Latitude: &lat, Longitude: &lng,
			},
			TripID: tripTestID,
		}},
	}))

	row, ok := items["cveh_trip_only"]
	if !ok {
		t.Fatalf("the trip-only vehicle is missing: %v", items)
	}
	if row["role"] != string(auth.RoleViewer) {
		t.Errorf("role = %v, want the wire value %q — never an internal RBAC name", row["role"], auth.RoleViewer)
	}
	// `location` is the field MYR-602 took OFF a plain viewer and gave only to
	// the two window-scoped roles. Its presence here is the proof the row was
	// projected through the elevated mask rather than the narrowed one.
	if row["location"] == nil {
		t.Fatalf("a trip participant's row carries no location: %v", row)
	}
	if row["activeTripId"] != tripTestID {
		t.Errorf("activeTripId = %v, want %q", row["activeTripId"], tripTestID)
	}
}

// TestTripRowsAreDedupedAgainstTheEarlierLegs.
//
// The rows already in hand win: an owner row carries the owner mask and a share
// row carries a real capability, and both say strictly more about the same car
// than a trip membership does.
func TestTripRowsAreDedupedAgainstTheEarlierLegs(t *testing.T) {
	items := catalogItems(t, WithTripVehicles(&fakeTripVehicleLister{
		active: map[string]string{tripTestVehicle: tripTestID},
		rows: []TripVehicleRow{{
			VehicleCatalogRow: VehicleCatalogRow{
				ID: tripTestVehicle, VIN: "7SAYGDET7TA613795", Name: "Roadie",
				Model: "Model Y", Year: 2024, Color: "UltraRed", Status: "parked",
			},
			TripID: tripTestID,
		}},
	}))

	if len(items) != 1 {
		t.Fatalf("the car appears %d times, want once", len(items))
	}
	// The OWNER row survived, not the trip row.
	if items[tripTestVehicle]["role"] != string(auth.RoleOwner) {
		t.Fatalf("role = %v, want the owner row to win the dedupe", items[tripTestVehicle]["role"])
	}
}

// TestActiveTripIDIsAllowedOnEveryCatalogRole is the tripwire the stamp's
// mask consultation depends on.
//
// vehicles_list_trip.go consults the NARROWEST role (viewer) and relies on the
// elevated lists being supersets of it. This asserts the three really do agree,
// so the conservative check is also the correct one — and that a future
// narrowing of `activeTripId` to fewer roles fails here rather than silently
// making the stamp inconsistent with the mask table.
func TestActiveTripIDIsAllowedOnEveryCatalogRole(t *testing.T) {
	for _, role := range auth.AllRoles() {
		if !mask.For(mask.ResourceVehicleSummary, role).Allows(activeTripIDField) {
			t.Errorf("role %s is denied %q — the catalog stamp consults the viewer mask "+
				"and assumes every role permits it", role, activeTripIDField)
		}
	}
}

// TestActiveTripIDIsOptionalInTheSchema is the other half of the absence rule.
//
// A field this projection may omit MUST be optional in
// vehicle-summary.schema.json, or every row without an open window is invalid
// against the shape its own consumer decodes — the exact failure
// TestViewerMaskKeepsEverySchemaRequiredField exists to catch in the other
// direction.
func TestActiveTripIDIsOptionalInTheSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contractsSchemaRoot(t), "vehicle-summary.schema.json")) //nolint:gosec // test-only read of a repo-relative contract schema
	if err != nil {
		t.Fatalf("read vehicle-summary.schema.json: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	summary, ok := doc.Defs["VehicleSummary"]
	if !ok {
		t.Fatal("no VehicleSummary definition in the vendored schema")
	}
	if _, declared := summary.Properties[activeTripIDField]; !declared {
		t.Fatalf("%q is emitted but not declared in the schema", activeTripIDField)
	}
	for _, r := range summary.Required {
		if r == activeTripIDField {
			t.Fatalf("%q is `required`, but this server omits it when there is no open window", activeTripIDField)
		}
	}
}

// TestATripUpGRADESTheShareRowRatherThanBeingSkipped is the regression this
// merge exists for, and the bug it was written after finding.
//
// EVERY TRIP PARTICIPANT IS BY CONSTRUCTION A SHARE-HOLDER — the picker's
// candidates ARE the car's accepted shares — so the share merge has already
// emitted a row for that car by the time the trip leg runs, projected through
// the plain-VIEWER mask, which since MYR-602 carries NO `location`.
//
// A dedupe that merely SKIPPED it (the member merge's rule, which is right for
// membership) would leave the participant holding a coordinate-less row for the
// entire window, and the client's per-row pickup ETA blank for the one car they
// were invited to watch. The row is replaced instead, and `sharePermission`
// survives the replacement because the trip row carries the caller's own
// AllowRides — a trip elevates the ROLE, it does not replace the relationship.
func TestATripUpgradesTheShareRowRatherThanBeingSkipped(t *testing.T) {
	lat, lng := 32.7767, -96.797
	shared := VehicleCatalogRow{
		ID: "cveh_shared", VIN: "7SAYGDET7TA888888", Name: "Nabil's car",
		Model: "Model S", Year: 2023, Color: "black", Status: "driving",
		Latitude: &lat, Longitude: &lng,
	}

	t.Run("without a trip the share row has no location", func(t *testing.T) {
		items := catalogItems(t,
			WithSharedVehicles(&fakeSharedLister{rows: []SharedVehicleRow{{VehicleCatalogRow: shared, AllowRides: true}}}),
		)
		row := items["cveh_shared"]
		if row["location"] != nil {
			t.Fatalf("a plain viewer's row carries a location: %v — MYR-602 narrowed that away", row["location"])
		}
	})

	t.Run("with an open window the same row gains it", func(t *testing.T) {
		items := catalogItems(t,
			WithSharedVehicles(&fakeSharedLister{rows: []SharedVehicleRow{{VehicleCatalogRow: shared, AllowRides: true}}}),
			WithTripVehicles(&fakeTripVehicleLister{
				active: map[string]string{"cveh_shared": tripTestID},
				rows: []TripVehicleRow{{
					VehicleCatalogRow: shared,
					TripID:            tripTestID,
					AllowRides:        true,
				}},
			}),
		)

		if len(items) != 2 {
			t.Fatalf("expected the owner's car and the shared one, got %d rows", len(items))
		}
		row := items["cveh_shared"]
		if row["location"] == nil {
			t.Fatalf("the trip did not upgrade the share row: %v", row)
		}
		// THE CAPABILITY MUST SURVIVE THE REPLACEMENT. A trip participant who
		// could request rides before the window opened must still be able to
		// during it; silently downgrading the tier would take a booking button
		// away at the moment the person is most likely to use it.
		if row["sharePermission"] != "rides" {
			t.Errorf("sharePermission = %v, want the share's own tier to survive", row["sharePermission"])
		}
		if row["activeTripId"] != tripTestID {
			t.Errorf("activeTripId = %v, want %q", row["activeTripId"], tripTestID)
		}
		// The WIRE role is unchanged — the elevation is in the field set, never
		// in the enum every shipped client decodes.
		if row["role"] != string(auth.RoleViewer) {
			t.Errorf("role = %v, want the wire value viewer", row["role"])
		}
	})
}
