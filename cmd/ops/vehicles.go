package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/myrobotaxi/telemetry/internal/store"
)

func runVehicles(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("vehicles requires a subcommand (list | re-add)")
	}
	switch args[0] {
	case "list":
		return runVehiclesList(ctx, args[1:])
	case "re-add":
		return runVehiclesReadd(ctx, args[1:])
	default:
		return fmt.Errorf("unknown vehicles subcommand %q", args[0])
	}
}

// vehicleListItem is the JSON shape printed for each vehicle.
type vehicleListItem struct {
	ID          string `json:"id"`
	VIN         string `json:"vin"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	ChargeLevel int    `json:"chargeLevel"`
	LastUpdated string `json:"lastUpdated"`
	// Access is MYR-599's operator label: `owner`, `driver`, or
	// `driver(unacknowledged)` — the last meaning the fleet-config push gate is
	// still shut on this car. Always present; on this surface it is never
	// `unknown`, because every row here came from a "Vehicle" row by
	// construction.
	Access string `json:"access"`
	// TeslaAccessType is Tesla's raw access_type token, printed only when a
	// driver-access row carries one. OMITTED for an owner car (no row to quote)
	// and for a driver row filed from an older Fleet API response that shipped
	// no access_type at all — an absent field says "Tesla told us nothing",
	// which is a different fact from `driver` and worth keeping distinguishable.
	TeslaAccessType string `json:"teslaAccessType,omitempty"`
}

func runVehiclesList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("vehicles list", flag.ContinueOnError)
	userID := fs.String("user-id", "", "MyRoboTaxi user id (Prisma cuid)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlag("user-id", *userID); err != nil {
		return err
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	// MYR-122: use the lean catalog projection — the ops CLI's
	// `vehicleListItem` only emits id/vin/name/status/chargeLevel/
	// lastUpdated, all of which are in the slim
	// `ListSummariesByUser` shape. No reason for the CLI to pull the
	// 37-column wide read (GPS dual-read + nav-route blob) when six
	// fields are projected out.
	repo := store.NewVehicleRepo(db.Pool(), store.NoopMetrics{})
	vehicles, err := repo.ListSummariesByUser(ctx, *userID)
	if err != nil {
		return fmt.Errorf("list vehicles: %w", err)
	}

	// MYR-599, in two halves and on purpose.
	//
	// The DERIVED half (is there a driver-access row, and is it acknowledged)
	// is ALREADY in the catalog projection as VehicleSummary.DriverAccess, so
	// the label costs nothing extra and is read from the same column pair the
	// app's own list endpoint reads — the CLI cannot disagree with the wire.
	//
	// The RAW half (Tesla's access_type token) is deliberately NOT in that
	// projection, for the reason internal/store/vehicle_driver_access.go states:
	// the REST list path is held to a lean projection and must not carry a
	// column no response body emits. Ops listings are allowed to be wide, so it
	// comes from ONE narrow extra read here rather than from widening the hot
	// query for every client in the fleet.
	rawAccess, err := repo.ListDriverAccessByVIN(ctx, *userID, vehicleVINs(vehicles))
	if err != nil {
		return fmt.Errorf("list driver access: %w", err)
	}

	return writeJSON(os.Stdout, vehicleListItems(vehicles, rawAccess))
}

// vehicleListItems renders the catalog rows as the CLI's JSON items, folding
// each car's driver-access answer in. Separated from runVehiclesList so the
// MYR-599 label mapping is testable without a database.
func vehicleListItems(
	vehicles []store.VehicleSummary, rawAccess map[string]store.DriverAccessListing,
) []vehicleListItem {
	out := make([]vehicleListItem, 0, len(vehicles))
	for i := range vehicles {
		v := &vehicles[i]
		// VehicleFound is asserted rather than taken from the lookup: this row
		// came FROM a "Vehicle" row, so `unknown` is not a state this surface
		// can be in, and a lookup miss must not silently downgrade a car whose
		// driver-access row the catalog already reported.
		listing := store.DriverAccessListing{
			VehicleDriverAccess: v.DriverAccess,
			VehicleFound:        true,
			TeslaAccessType:     rawAccess[v.VIN].TeslaAccessType,
		}
		out = append(out, vehicleListItem{
			ID:              v.ID,
			VIN:             v.VIN,
			Name:            v.Name,
			Status:          string(v.Status),
			ChargeLevel:     v.ChargeLevel,
			LastUpdated:     v.LastUpdated.UTC().Format("2006-01-02T15:04:05Z07:00"),
			Access:          listing.OperatorLabel(),
			TeslaAccessType: listing.TeslaAccessType,
		})
	}
	return out
}

// vehicleVINs collects the VINs of a summary page for the driver-access lookup.
// Full VINs, matching what this subcommand already prints: `ops vehicles list`
// is an operator tool whose whole output is the row, and a redacted key could
// not be joined back to it.
func vehicleVINs(vehicles []store.VehicleSummary) []string {
	out := make([]string, 0, len(vehicles))
	for i := range vehicles {
		out = append(out, vehicles[i].VIN)
	}
	return out
}

// vehicleReaddOutput is the JSON printed by `ops vehicles re-add`.
type vehicleReaddOutput struct {
	UserID         string `json:"userId"`
	TeslaVehicleID string `json:"teslaVehicleId"`
	// Cleared reports whether a removed-vehicle tombstone was actually present
	// and cleared. False means there was no tombstone (a clean idempotent no-op).
	Cleared bool `json:"cleared"`
}

// runVehiclesReadd is the operator stopgap for MYR-262: un-tombstone a specific
// (userID, teslaVehicleId) on request — useful before the app-side re-add UI
// lands and for support. It clears ONLY the given owner's tombstone
// (RemovedVehicleRegistry.ClearTombstone is owner-scoped and idempotent, and
// writes the vehicle_readd_allowed audit row on an actual clear). It does NOT
// provision the car — the operator/owner triggers the next Tesla sync (a
// re-link, or `ops fleet-config push`) which now, with the tombstone gone,
// provisions the VIN normally.
func runVehiclesReadd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("vehicles re-add", flag.ContinueOnError)
	userID := fs.String("user-id", "", "MyRoboTaxi user id (owner of the removed car)")
	teslaVehicleID := fs.String("tesla-vehicle-id", "", "Tesla vehicle id (the go_removed_vehicles tombstone key)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlag("user-id", *userID); err != nil {
		return err
	}
	if err := requireFlag("tesla-vehicle-id", *teslaVehicleID); err != nil {
		return err
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	registry := store.NewRemovedVehicleRegistry(db.Pool(), logger)
	cleared, err := registry.ClearTombstone(ctx, *userID, *teslaVehicleID)
	if err != nil {
		return fmt.Errorf("clear tombstone: %w", err)
	}

	return writeJSON(os.Stdout, vehicleReaddOutput{
		UserID:         *userID,
		TeslaVehicleID: *teslaVehicleID,
		Cleared:        cleared,
	})
}
