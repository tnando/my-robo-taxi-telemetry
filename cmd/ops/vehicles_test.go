package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestRunVehiclesReadd_FlagValidation pins the `ops vehicles re-add` argument
// contract (MYR-262) without touching a database: both --user-id and
// --tesla-vehicle-id are required, and they are validated BEFORE any DB
// connection is attempted (so a missing flag never opens a pool).
func TestRunVehiclesReadd_FlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing both flags errors on user-id first",
			args:    nil,
			wantErr: "--user-id is required",
		},
		{
			name:    "missing tesla-vehicle-id errors",
			args:    []string{"--user-id", "cuser1"},
			wantErr: "--tesla-vehicle-id is required",
		},
		{
			name:    "unknown flag errors from the flag set",
			args:    []string{"--nope", "x"},
			wantErr: "flag provided but not defined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runVehiclesReadd(context.Background(), tt.args)
			if err == nil {
				t.Fatalf("runVehiclesReadd(%v) = nil, want error %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestRunVehicles_Dispatch verifies the `re-add` subcommand is routed and that
// an unknown subcommand is rejected.
func TestRunVehicles_Dispatch(t *testing.T) {
	// Unknown subcommand.
	if err := runVehicles(context.Background(), []string{"bogus"}); err == nil ||
		!strings.Contains(err.Error(), "unknown vehicles subcommand") {
		t.Errorf("runVehicles(bogus) error = %v, want unknown subcommand", err)
	}
	// re-add with no flags routes to runVehiclesReadd and fails flag validation
	// (proving it is dispatched, not rejected as unknown).
	if err := runVehicles(context.Background(), []string{"re-add"}); err == nil ||
		!strings.Contains(err.Error(), "--user-id is required") {
		t.Errorf("runVehicles(re-add) error = %v, want flag-validation error", err)
	}
}

// TestVehicleReaddOutput_JSONShape pins the ops re-add JSON wire shape.
func TestVehicleReaddOutput_JSONShape(t *testing.T) {
	b, err := json.Marshal(vehicleReaddOutput{
		UserID:         "cuser1",
		TeslaVehicleID: "vid-9",
		Cleared:        true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"userId":"cuser1"`, `"teslaVehicleId":"vid-9"`, `"cleared":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("json = %s, want it to contain %s", got, want)
		}
	}
}

// TestVehicleListItems_Access pins MYR-599's access column on `ops vehicles
// list`: the label comes from the catalog's own driver-access row and the raw
// Tesla token from the narrow ops-only lookup, keyed by VIN.
//
// `unknown` is deliberately absent from this table — every row on this surface
// came from a "Vehicle" row, so the only surface that can report it is the
// orphan sweep.
func TestVehicleListItems_Access(t *testing.T) {
	tests := []struct {
		name    string
		access  store.VehicleDriverAccess
		raw     map[string]store.DriverAccessListing
		want    string
		wantRaw string
	}{
		{
			name: "no driver-access row is owner and quotes nothing",
			want: store.OperatorAccessOwner,
		},
		{
			name:   "acknowledged driver row",
			access: store.VehicleDriverAccess{Present: true, CreatedAt: time.Unix(1, 0), AcknowledgedAt: time.Unix(2, 0)},
			raw: map[string]store.DriverAccessListing{
				"VIN-1": {TeslaAccessType: "DRIVER"},
			},
			want:    store.OperatorAccessDriver,
			wantRaw: "DRIVER",
		},
		{
			name:    "unacknowledged driver row keeps the shut push gate visible",
			access:  store.VehicleDriverAccess{Present: true, CreatedAt: time.Unix(1, 0)},
			raw:     map[string]store.DriverAccessListing{"VIN-1": {TeslaAccessType: "DRIVER"}},
			want:    store.OperatorAccessDriverUnacknowledged,
			wantRaw: "DRIVER",
		},
		{
			name:   "driver row from an older Fleet response quotes no token",
			access: store.VehicleDriverAccess{Present: true, CreatedAt: time.Unix(1, 0)},
			raw:    map[string]store.DriverAccessListing{"VIN-1": {TeslaAccessType: ""}},
			want:   store.OperatorAccessDriverUnacknowledged,
		},
		{
			name:   "a raw-lookup miss never downgrades the catalog's answer",
			access: store.VehicleDriverAccess{Present: true, CreatedAt: time.Unix(1, 0), AcknowledgedAt: time.Unix(2, 0)},
			raw:    nil,
			want:   store.OperatorAccessDriver,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vehicleListItems([]store.VehicleSummary{{
				ID: "cveh1", VIN: "VIN-1", DriverAccess: tt.access,
			}}, tt.raw)
			if len(got) != 1 {
				t.Fatalf("rendered %d items, want 1", len(got))
			}
			if got[0].Access != tt.want {
				t.Errorf("access = %q, want %q", got[0].Access, tt.want)
			}
			if got[0].TeslaAccessType != tt.wantRaw {
				t.Errorf("teslaAccessType = %q, want %q", got[0].TeslaAccessType, tt.wantRaw)
			}

			b, err := json.Marshal(got[0])
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(b), `"access":"`+tt.want+`"`) {
				t.Errorf("json = %s, want the access label", b)
			}
			// The raw token is OMITTED rather than emitted empty: "Tesla told
			// us nothing" must stay distinguishable from a value.
			if tt.wantRaw == "" && strings.Contains(string(b), "teslaAccessType") {
				t.Errorf("json = %s, want no teslaAccessType key", b)
			}
		})
	}
}

// TestVehicleVINs pins the lookup key list — full VINs, in row order.
func TestVehicleVINs(t *testing.T) {
	got := vehicleVINs([]store.VehicleSummary{{VIN: "VIN-A"}, {VIN: "VIN-B"}})
	if len(got) != 2 || got[0] != "VIN-A" || got[1] != "VIN-B" {
		t.Errorf("vehicleVINs = %v, want [VIN-A VIN-B]", got)
	}
}
