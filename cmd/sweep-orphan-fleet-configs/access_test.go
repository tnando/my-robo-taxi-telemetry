package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/fleetorphan"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// fakeDriverAccessLister answers from a per-owner canned map, recording the
// VINs it was asked for so a test can assert the batching.
type fakeDriverAccessLister struct {
	byUser map[string]map[string]store.DriverAccessListing
	err    error
	calls  map[string][]string
}

func (f *fakeDriverAccessLister) ListDriverAccessByVIN(
	_ context.Context, userID string, vins []string,
) (map[string]store.DriverAccessListing, error) {
	if f.calls == nil {
		f.calls = map[string][]string{}
	}
	f.calls[userID] = append(f.calls[userID], vins...)
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

func ack(t time.Time) store.DriverAccessListing {
	return store.DriverAccessListing{
		VehicleDriverAccess: store.VehicleDriverAccess{
			Present:        true,
			CreatedAt:      time.Unix(1, 0),
			AcknowledgedAt: t,
		},
		VehicleFound:    true,
		TeslaAccessType: "DRIVER",
	}
}

// TestAnnotateDriverAccess covers the four labels an orphan line can carry,
// including the one that matters most on this report: a VIN with no local
// vehicle row is `unknown` and NEVER `owner`.
func TestAnnotateDriverAccess(t *testing.T) {
	tests := []struct {
		name     string
		line     fleetorphan.VINOutcome
		listings map[string]store.DriverAccessListing
		want     string
		wantRaw  string
	}{
		{
			name:     "no vehicle row is unknown, not owner",
			line:     fleetorphan.VINOutcome{VIN: "VIN-GONE", UserID: "cuser1"},
			listings: nil,
			want:     store.OperatorAccessUnknown,
		},
		{
			name: "vehicle row with no driver access is owner",
			line: fleetorphan.VINOutcome{VIN: "VIN-OWN", UserID: "cuser1"},
			listings: map[string]store.DriverAccessListing{
				"VIN-OWN": {VehicleFound: true},
			},
			want: store.OperatorAccessOwner,
		},
		{
			name:     "acknowledged driver row",
			line:     fleetorphan.VINOutcome{VIN: "VIN-DRV", UserID: "cuser1"},
			listings: map[string]store.DriverAccessListing{"VIN-DRV": ack(time.Unix(2, 0))},
			want:     store.OperatorAccessDriver,
			wantRaw:  "DRIVER",
		},
		{
			name:     "unacknowledged driver row keeps the shut gate visible",
			line:     fleetorphan.VINOutcome{VIN: "VIN-PEND", UserID: "cuser1"},
			listings: map[string]store.DriverAccessListing{"VIN-PEND": ack(time.Time{})},
			want:     store.OperatorAccessDriverUnacknowledged,
			wantRaw:  "DRIVER",
		},
		{
			name: "driver row from an older Fleet response quotes no token",
			line: fleetorphan.VINOutcome{VIN: "VIN-OLD", UserID: "cuser1"},
			listings: map[string]store.DriverAccessListing{
				"VIN-OLD": {
					VehicleDriverAccess: store.VehicleDriverAccess{Present: true, CreatedAt: time.Unix(1, 0)},
					VehicleFound:        true,
				},
			},
			want: store.OperatorAccessDriverUnacknowledged,
		},
		{
			name: "a line with no owner handle is not looked up",
			line: fleetorphan.VINOutcome{VIN: "VIN-NOUSER"},
			want: store.OperatorAccessUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeDriverAccessLister{
				byUser: map[string]map[string]store.DriverAccessListing{"cuser1": tt.listings},
			}
			got := annotateDriverAccess(context.Background(), lister,
				fleetorphan.Report{VINs: []fleetorphan.VINOutcome{tt.line}})

			if len(got.VINs) != 1 {
				t.Fatalf("annotated %d lines, want 1", len(got.VINs))
			}
			if got.VINs[0].Access != tt.want {
				t.Errorf("access = %q, want %q", got.VINs[0].Access, tt.want)
			}
			if got.VINs[0].TeslaAccessType != tt.wantRaw {
				t.Errorf("teslaAccessType = %q, want %q", got.VINs[0].TeslaAccessType, tt.wantRaw)
			}
			if got.VINs[0].VIN != tt.line.VIN {
				t.Errorf("VIN = %q, want %q — the original line must survive annotation",
					got.VINs[0].VIN, tt.line.VIN)
			}
			if tt.line.UserID == "" && len(lister.calls) != 0 {
				t.Errorf("looked up %v for a line with no owner handle", lister.calls)
			}
			if len(got.Errors) != 0 {
				t.Errorf("errors = %v, want none", got.Errors)
			}
		})
	}
}

// TestAnnotateDriverAccess_Batching pins the one-read-per-owner behaviour and
// the owner+VIN keying: the same VIN reported for two owners must not have one
// owner's answer written onto the other's line.
func TestAnnotateDriverAccess_Batching(t *testing.T) {
	lister := &fakeDriverAccessLister{
		byUser: map[string]map[string]store.DriverAccessListing{
			"cuserA": {"VIN-SHARED": ack(time.Time{})},
			"cuserB": {},
		},
	}
	rep := fleetorphan.Report{VINs: []fleetorphan.VINOutcome{
		{VIN: "VIN-SHARED", UserID: "cuserA"},
		{VIN: "VIN-SHARED", UserID: "cuserB"},
		{VIN: "VIN-OTHER", UserID: "cuserA"},
	}}

	got := annotateDriverAccess(context.Background(), lister, rep)

	if len(lister.calls) != 2 {
		t.Fatalf("made %d owner reads, want 2 (one per owner)", len(lister.calls))
	}
	if len(lister.calls["cuserA"]) != 2 {
		t.Errorf("cuserA read %v, want both of its VINs in one batch", lister.calls["cuserA"])
	}
	if got.VINs[0].Access != store.OperatorAccessDriverUnacknowledged {
		t.Errorf("cuserA line = %q, want the driver answer", got.VINs[0].Access)
	}
	if got.VINs[1].Access != store.OperatorAccessUnknown {
		t.Errorf("cuserB line = %q, want unknown — cuserA's row is not its answer", got.VINs[1].Access)
	}
}

// TestAnnotateDriverAccess_LookupFailure: a failed lookup is reported, never
// fatal, and never turns a line into a confident answer.
func TestAnnotateDriverAccess_LookupFailure(t *testing.T) {
	lister := &fakeDriverAccessLister{err: errors.New(strings.Repeat("x", errorTextCap+50))}
	got := annotateDriverAccess(context.Background(), lister,
		fleetorphan.Report{VINs: []fleetorphan.VINOutcome{{VIN: "VIN-1", UserID: "cuser1"}}})

	if len(got.VINs) != 1 || got.VINs[0].Access != store.OperatorAccessUnknown {
		t.Fatalf("lines = %+v, want one unknown line", got.VINs)
	}
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0], "list driver access") {
		t.Fatalf("errors = %v, want one driver-access error", got.Errors)
	}
	if !strings.HasSuffix(got.Errors[0], "…(truncated)") {
		t.Errorf("error was not capped: %q", got.Errors[0])
	}
}

// TestAccessReport_JSONShape pins the shadowing that the whole accessReport
// type rests on: exactly ONE "vins" key, carrying the annotated lines, with
// every other field of fleetorphan.Report promoted through untouched.
func TestAccessReport_JSONShape(t *testing.T) {
	rep := fleetorphan.Report{
		DryRun: true,
		Counts: map[string]int{fleetorphan.OutcomeNoConfig: 1},
		VINs:   []fleetorphan.VINOutcome{{VIN: "VIN-1", UserID: "cuser1", Outcome: fleetorphan.OutcomeNoConfig}},
	}
	lister := &fakeDriverAccessLister{
		byUser: map[string]map[string]store.DriverAccessListing{
			"cuser1": {"VIN-1": ack(time.Time{})},
		},
	}

	b, err := json.Marshal(annotateDriverAccess(context.Background(), lister, rep))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)

	if n := strings.Count(got, `"vins"`); n != 1 {
		t.Fatalf("json has %d \"vins\" keys, want 1 (the shadowing broke): %s", n, got)
	}
	for _, want := range []string{
		`"dryRun":true`,
		`"vin":"VIN-1"`,
		`"outcome":"no_config"`,
		`"access":"driver(unacknowledged)"`,
		`"teslaAccessType":"DRIVER"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("json = %s, want it to contain %s", got, want)
		}
	}

	// The raw token is omitted rather than emitted empty when there is nothing
	// to quote — "Tesla said nothing" must not read as a value.
	plain, err := json.Marshal(annotateDriverAccess(context.Background(),
		&fakeDriverAccessLister{}, rep))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "teslaAccessType") {
		t.Errorf("json = %s, want no teslaAccessType key", plain)
	}
	if !strings.Contains(string(plain), `"access":"unknown"`) {
		t.Errorf("json = %s, want the unknown label", plain)
	}
}
