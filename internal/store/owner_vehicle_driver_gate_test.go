package store_test

// MYR-599 — WHEN THE CONSENT GATE MAY BE WRITTEN, against a real database.
//
// The gate's fail-closed rule (anything Tesla says that is not "OWNER", the
// EMPTY STRING included, counts as DRIVER) is correct at the boundary and
// destructive if applied without bound. These tests pin the bound.
//
// The failure they exist to prevent is not hypothetical arithmetic: older Fleet
// API responses have shipped no `access_type` at all. One such listing arriving
// for a car its real owner has been streaming for months would, under an
// unbounded rule, file a driver-access row against it — and from that instant
// every push path refuses the car, the reconciler stops repairing it, the
// inactivity sweeper stops counting it, and the owner's app shows a sheet asking
// them to confirm that somebody else approved adding their own vehicle. The only
// exit is an acknowledgment that would be a lie for them to sign.

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

const (
	gateUser = "cgate_user001"
	gateVID  = "vid-gate-1"
	gateVIN  = "5YJ3E1EA7KF000701"
)

func TestOwnerProvisioner_DriverGateBounds(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping driver-gate integration test")
	}
	ctx := context.Background()
	ensureOwnerSchema(t)
	mustApplyGoMigrations(t)
	prov := newTestProvisioner(t)

	gateRows := func(t *testing.T) int {
		t.Helper()
		return countQuery(t, `SELECT count(*) FROM go_vehicle_driver_access`)
	}

	tests := []struct {
		name string
		// seedExisting, when set, provisions the car FIRST under this access
		// type, so the second call lands on an ESTABLISHED row rather than
		// inserting one.
		seedExisting *string
		accessType   string
		access       store.TeslaAccessSignal
		wantRows     int
		// wantAccessType is what tesla_access_type must hold afterwards, when a
		// row is expected. THE EMPTY STRING IS A REAL EXPECTATION: older Fleet
		// responses have shipped an absent access_type, and inventing "DRIVER"
		// here would erase the one thing this column exists for — answering,
		// later, what Tesla actually said.
		wantAccessType string
		wantPending    bool
		wantDowngade   bool
		because        string
	}{
		{
			name:           "an EMPTY access_type on a NEW car writes the gate",
			accessType:     "",
			access:         store.AccessSignalFor(""),
			wantRows:       1,
			wantAccessType: "",
			wantPending:    true,
			because: "nothing is established, nobody is streaming, and an unknown access level " +
				"on a first sighting is exactly what fail-closed exists for",
		},
		{
			name:           "a DRIVER access_type on a NEW car writes the gate",
			accessType:     "DRIVER",
			access:         store.AccessSignalDriver,
			wantRows:       1,
			wantAccessType: "DRIVER",
			wantPending:    true,
			because:        "the ordinary MYR-599 case",
		},
		{
			name:         "an EMPTY access_type on an ESTABLISHED OWNER row writes NOTHING",
			seedExisting: strPtr("OWNER"),
			accessType:   "",
			access:       store.AccessSignalFor(""),
			wantRows:     0,
			wantDowngade: true,
			because: "THIS IS THE LOCKOUT. One Fleet listing that omitted access_type would " +
				"otherwise gate a car its owner has been streaming for months, and the only " +
				"way out is an acknowledgment that is a lie for them to sign",
		},
		{
			name:         "a DRIVER access_type on an ESTABLISHED OWNER row also writes NOTHING",
			seedExisting: strPtr("OWNER"),
			accessType:   "DRIVER",
			access:       store.AccessSignalDriver,
			wantRows:     0,
			wantDowngade: true,
			because: "a TRUE access downgrade is explicitly out of scope for MYR-599; what is " +
				"recorded is that the signal was seen and refused, not a gate on a live car",
		},
		{
			name:           "a re-link of an EXISTING driver car refreshes its gate",
			seedExisting:   strPtr("DRIVER"),
			accessType:     "DRIVER_2",
			access:         store.AccessSignalDriver,
			wantRows:       1,
			wantAccessType: "DRIVER_2",
			wantPending:    true,
			because: "the row already carries a gate, so this is a refresh rather than a " +
				"conversion — the bound is on CREATING one, not on maintaining one, and the " +
				"refresh must record what Tesla NOW says",
		},
		{
			name:         "an OWNER re-link of a driver car CLEARS the gate",
			seedExisting: strPtr("DRIVER"),
			accessType:   "OWNER",
			access:       store.AccessSignalOwner,
			wantRows:     0,
			because:      "the access-UPGRADE case; a stale row would hold the push gate shut on a car needing nobody's permission",
		},
		{
			name:           "an UNKNOWN signal touches nothing and reports the gate honestly",
			seedExisting:   strPtr("DRIVER"),
			accessType:     "",
			access:         store.AccessSignalUnknown,
			wantRows:       1,
			wantAccessType: "DRIVER",
			wantPending:    true,
			because: "the caller made no claim, so there is no basis for changing a consent " +
				"fact — not even the access type — but 'we did not look' must not be " +
				"REPORTED as 'there is no gate'",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanOwnerTables(t)
			if _, err := testPool.Exec(ctx, `DELETE FROM go_vehicle_driver_access`); err != nil {
				t.Fatalf("clean gate table: %v", err)
			}
			if _, err := testPool.Exec(ctx, `DELETE FROM "Vehicle"`); err != nil {
				t.Fatalf("clean vehicles: %v", err)
			}
			seedOwnerUser(t, gateUser, "", "")

			in := store.OwnedVehicleInput{
				UserID:         gateUser,
				TeslaVehicleID: gateVID,
				VIN:            gateVIN,
				Name:           "Gate test",
			}
			if tc.seedExisting != nil {
				seed := in
				seed.TeslaAccessType = *tc.seedExisting
				seed.Access = store.AccessSignalFor(*tc.seedExisting)
				if _, err := prov.UpsertOwnedVehicle(ctx, seed); err != nil {
					t.Fatalf("seed existing car (%d): %v", i, err)
				}
			}

			in.TeslaAccessType = tc.accessType
			in.Access = tc.access
			out, err := prov.UpsertOwnedVehicle(ctx, in)
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}
			if out.Outcome != store.VehicleOwned {
				t.Fatalf("outcome = %q, want owned", out.Outcome)
			}
			if got := gateRows(t); got != tc.wantRows {
				t.Errorf("driver-access rows = %d, want %d (%s)", got, tc.wantRows, tc.because)
			}
			if tc.wantRows == 1 {
				var got string
				if err := testPool.QueryRow(ctx,
					`SELECT tesla_access_type FROM go_vehicle_driver_access`).Scan(&got); err != nil {
					t.Fatalf("read tesla_access_type: %v", err)
				}
				if got != tc.wantAccessType {
					t.Errorf("tesla_access_type = %q, want Tesla's own %q — this column exists "+
						"to answer what Tesla actually said", got, tc.wantAccessType)
				}
			}
			if out.DriverAccessPending != tc.wantPending {
				t.Errorf("DriverAccessPending = %v, want %v (%s)",
					out.DriverAccessPending, tc.wantPending, tc.because)
			}
			if out.AccessDowngradeObserved != tc.wantDowngade {
				t.Errorf("AccessDowngradeObserved = %v, want %v — the refusal must be visible "+
					"to the caller, because it is either a Fleet listing that omitted "+
					"access_type or a real downgrade nobody has built handling for",
					out.AccessDowngradeObserved, tc.wantDowngade)
			}
		})
	}
}

// TestOwnerProvisioner_DriverGateKeepsAcknowledgment pins the one thing a
// re-link must never do: re-shut a gate the person already opened. Without it
// every incidental AfterLink would demand a second acknowledgment for a car that
// is already streaming.
func TestOwnerProvisioner_DriverGateKeepsAcknowledgment(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping driver-gate integration test")
	}
	ctx := context.Background()
	ensureOwnerSchema(t)
	mustApplyGoMigrations(t)
	cleanOwnerTables(t)
	if _, err := testPool.Exec(ctx, `DELETE FROM go_vehicle_driver_access`); err != nil {
		t.Fatalf("clean gate table: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM "Vehicle"`); err != nil {
		t.Fatalf("clean vehicles: %v", err)
	}
	seedOwnerUser(t, gateUser, "", "")
	prov := newTestProvisioner(t)

	in := store.OwnedVehicleInput{
		UserID: gateUser, TeslaVehicleID: gateVID, VIN: gateVIN, Name: "Borrowed",
		TeslaAccessType: "DRIVER", Access: store.AccessSignalDriver,
	}
	first, err := prov.UpsertOwnedVehicle(ctx, in)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !first.DriverAccessPending {
		t.Fatal("a freshly written gate must be reported as SHUT")
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE go_vehicle_driver_access SET acknowledged_at = NOW(), acknowledgment_version = 'owner-approval-v1'
		 WHERE vehicle_id = $1`, first.VehicleID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	second, err := prov.UpsertOwnedVehicle(ctx, in)
	if err != nil {
		t.Fatalf("re-link: %v", err)
	}
	if !second.DriverAccessPresent {
		t.Error("DriverAccessPresent = false after a re-link — the car is still a driver's car, " +
			"and teslaAccessType must keep saying so")
	}
	if second.DriverAccessPending {
		t.Error("DriverAccessPending = true after a re-link of an ACKNOWLEDGED car — consent, " +
			"once given, is not withdrawn by a background sync, and reporting the gate as " +
			"shut here is what made the link hook re-seed awaiting_owner_ack forever")
	}
}
