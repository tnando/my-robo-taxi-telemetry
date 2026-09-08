package main

import (
	"context"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// The CATALOG side of the sharing boundary: the §7.0 reads that answer "which
// cars can this person see, and what do they look like" for a viewer or a
// group-ride member. Split from share_adapters.go under the 300-line rule when
// MYR-609 added to that file; the seam is a real one — everything left there
// mutates or reads INVITES, everything here projects VEHICLES, and the two
// share only the store repository they sit in front of.

// sharedVehicleListerAdapter binds the viewer-side catalog reads to
// telemetry.SharedVehicleLister.
type sharedVehicleListerAdapter struct {
	repo *store.VehicleRepo
}

// ListSharedByUser returns every vehicle shared with the caller.
func (a *sharedVehicleListerAdapter) ListSharedByUser(ctx context.Context, userID string) ([]telemetry.SharedVehicleRow, error) {
	rows, err := a.repo.ListSharedSummariesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return toSharedVehicleRows(rows), nil
}

// ListSharedByIDs narrows the same access-checked query to one redemption's set.
func (a *sharedVehicleListerAdapter) ListSharedByIDs(ctx context.Context, userID string, vehicleIDs []string) ([]telemetry.SharedVehicleRow, error) {
	rows, err := a.repo.ListSharedSummariesByIDs(ctx, userID, vehicleIDs)
	if err != nil {
		return nil, err
	}
	return toSharedVehicleRows(rows), nil
}

// toSharedVehicleRows converts store rows to the handler's catalog shape.
func toSharedVehicleRows(rows []store.SharedVehicleSummary) []telemetry.SharedVehicleRow {
	out := make([]telemetry.SharedVehicleRow, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, telemetry.SharedVehicleRow{
			VehicleCatalogRow: telemetry.VehicleCatalogRow{
				ID:                   row.ID,
				VIN:                  row.VIN,
				Name:                 row.Name,
				Model:                row.Model,
				Year:                 row.Year,
				Color:                row.Color,
				LicensePlate:         row.LicensePlate,
				Status:               string(row.Status),
				ChargeLevel:          row.ChargeLevel,
				EstimatedRange:       row.EstimatedRange,
				LastUpdated:          row.LastUpdated,
				HasActiveRide:        row.HasActiveRide,
				ServiceETC:           row.ServiceETC,
				ServiceExpectedEndAt: row.ServiceExpectedEndAt,
				// MYR-342: viewers see the pause too — the whole point is that a
				// rider learns the shared car is not taking requests from the
				// catalog, not from a 409.
				RideShareEnabled: row.RideShareEnabled,
				// MYR-507: viewers see the trim too, and for the sharpest
				// version of the argument on this struct — the viewer is the
				// ONLY party who needs it. An owner reads the trim off their own
				// /snapshot; a rider never fetches one, so this row is where a
				// shared car gets to say it is a Plaid rather than an "UltraRed".
				TrimLabel: row.TrimLabel,
				Trim:      row.Trim,
				// MYR-581: viewers see the owner's first name, and this is the
				// role the field was added for — the whole report was a rider
				// being shown "Tesla" where a person's name belonged. Owners get
				// it too (their own row names them), so all three §7.0 producers
				// share one projection.
				OwnerFirstName: row.OwnerFirstName,
				// MYR-592 — carried on viewer and member rows too.
				TelemetrySuspendedAt: row.TelemetrySuspendedAt,
				// MYR-515: viewers see the position too — the same value the
				// viewer mask already retains on the streaming path for these
				// very cars, which is what makes the picker's per-row pickup
				// ETA possible for a car the client is not watching.
				Latitude:  row.Latitude,
				Longitude: row.Longitude,
				// MYR-491: viewers see the setup state too, and for the sharper
				// version of the same argument — MYR-437's picker must show a
				// shared car as "setting up" rather than silently omitting it or
				// badging a never-streamed car "offline".
				SetupSchedule: setupScheduleRow(row.SetupSchedule),
				// MYR-599: viewers and group-ride members see
				// `teslaAccessType` too — the party meeting a car their friend
				// DRIVES rather than owns is the one most helped by knowing
				// that access rests on somebody else's permission.
				DriverAccess: driverAccessRow(row.DriverAccess),
			},
			AllowRides: row.AllowRides,
		})
	}
	return out
}

// memberVehicleListerAdapter binds the MYR-540 group-ride member catalog read
// to telemetry.RideMemberVehicleLister.
type memberVehicleListerAdapter struct {
	repo *store.VehicleRepo
}

// ListMemberVehiclesByUser returns the vehicles of live group rides the caller
// has joined. The store rows arrive in the shared-summary shape (the query
// reuses that projection with a literal FALSE capability — membership conveys
// the zero grant); only the catalog row survives the boundary, exactly because
// there is no capability to carry.
func (a *memberVehicleListerAdapter) ListMemberVehiclesByUser(ctx context.Context, userID string) ([]telemetry.VehicleCatalogRow, error) {
	rows, err := a.repo.ListMemberVehicleSummaries(ctx, userID)
	if err != nil {
		return nil, err
	}
	shared := toSharedVehicleRows(rows)
	out := make([]telemetry.VehicleCatalogRow, 0, len(shared))
	for i := range shared {
		out = append(out, shared[i].VehicleCatalogRow)
	}
	return out, nil
}
