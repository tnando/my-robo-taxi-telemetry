package mask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// TestSentinelsAreExactlyTheRequiredLocationFields is the load-bearing test in
// this file, and it reads the SCHEMA rather than a copy of it.
//
// The sentinel list is defined as one intersection: the fields
// vehicle-state.schema.json marks `required` AND the fields MYR-602 moved off
// the plain viewer. Both sides can move independently — a new required field, a
// field promoted into or out of the live-location group — and either move
// silently breaks the property this whole mechanism exists for. Computing the
// intersection here and comparing it to the hand-written list is what turns
// that into a failing test instead of a viewer receiving an invalid frame.
func TestSentinelsAreExactlyTheRequiredLocationFields(t *testing.T) {
	required := schemaRequiredFields(t)

	var want []string
	for _, field := range vehicleStateLiveLocationFields {
		if slices.Contains(required, field) {
			want = append(want, field)
		}
	}
	slices.Sort(want)

	got := RequiredLocationSentinelFields()
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("sentinel fields = %v, want %v — the intersection of "+
			"vehicle-state.schema.json's `required` list with the live-location group "+
			"moved, and the sentinel table did not follow it", got, want)
	}
	for _, field := range got {
		if _, ok := requiredLocationSentinels[field]; !ok {
			t.Errorf("field %q is in the ordered list but has no sentinel VALUE", field)
		}
	}
}

// schemaRequiredFields reads the contract's own `required` array.
func schemaRequiredFields(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "contracts", "schemas", "vehicle-state.schema.json")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative contract path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	if len(doc.Required) == 0 {
		t.Fatalf("%s declares no required fields; the intersection would be vacuous", path)
	}
	return doc.Required
}

func TestApplyLocationSentinels(t *testing.T) {
	tests := []struct {
		name    string
		role    auth.Role
		input   map[string]any
		want    map[string]any
		wantFil []string
	}{
		{
			name:  "viewer: masked required keys come back as sentinels",
			role:  auth.RoleViewer,
			input: map[string]any{"latitude": 37.7, "longitude": -122.4, "chargeLevel": float64(80)},
			want: map[string]any{
				"chargeLevel": float64(80),
				"latitude":    float64(0),
				"longitude":   float64(0),
			},
			wantFil: []string{"latitude", "longitude"},
		},
		{
			name:  "viewer: a key the frame never carried is NOT invented",
			role:  auth.RoleViewer,
			input: map[string]any{"chargeLevel": float64(80)},
			want:  map[string]any{"chargeLevel": float64(80)},
		},
		{
			name:  "viewer: optional navigation stays absent",
			role:  auth.RoleViewer,
			input: map[string]any{"destinationName": "Home", "chargeLevel": float64(80)},
			want:  map[string]any{"chargeLevel": float64(80)},
		},
		{
			name:  "trip participant: real values survive untouched",
			role:  auth.RoleTripParticipant,
			input: map[string]any{"latitude": 37.7, "speed": float64(42)},
			want:  map[string]any{"latitude": 37.7, "speed": float64(42)},
		},
		{
			name:  "deny-all sentinel role gets nothing, not zeros",
			role:  auth.Role(""),
			input: map[string]any{"latitude": 37.7},
			want:  map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projected, _ := Apply(tt.input, For(ResourceVehicleState, tt.role))
			got, filled := ApplyLocationSentinels(tt.input, projected, tt.role)

			if len(got) != len(tt.want) {
				t.Fatalf("projection = %v, want %v", got, tt.want)
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("field %q = %v, want %v (whole projection %v)", k, got[k], want, got)
				}
			}
			slices.Sort(filled)
			slices.Sort(tt.wantFil)
			if !slices.Equal(filled, tt.wantFil) {
				t.Errorf("filled = %v, want %v", filled, tt.wantFil)
			}
		})
	}
}

// TestApplyLocationSentinels_DoesNotMutateInput pins the contract Apply already
// keeps: a caller may hold the source payload across several role projections in
// one broadcast, and a sentinel written back into it would redact the OWNER'S
// frame on the next iteration.
func TestApplyLocationSentinels_DoesNotMutateInput(t *testing.T) {
	input := map[string]any{"latitude": 37.7, "longitude": -122.4}
	projected, _ := Apply(input, For(ResourceVehicleState, auth.RoleViewer))
	ApplyLocationSentinels(input, projected, auth.RoleViewer)

	if input["latitude"] != 37.7 || input["longitude"] != -122.4 {
		t.Errorf("input was mutated: %v", input)
	}
}
