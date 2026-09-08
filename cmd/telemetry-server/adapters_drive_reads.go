package main

import (
	"context"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// THE TWO DRIVE-SCOPED READ ADAPTERS — §7.3 (drive detail) and §7.4 (drive
// route) — and the ONE producer of the access identity they share.
//
// Split out of adapters.go (MYR-614). That file is 500+ lines, well past
// CLAUDE.md's 300-line cap, and this package already splits its adapters by
// concern (adapters_vehicle_snapshot.go, trip_adapters.go, share_adapters.go,
// ride_request_adapters.go, account_deletion_adapters.go). Adding a self
// contained concern to the file that is already over the cap is how the cap
// stops being reachable; the drive reads are the concern this issue is about,
// so they are the ones that move. Paired with wiring_drive_reads_test.go, which
// drives these adapters through the real handlers.

// driveRecordReader is the one store method the two drive-scoped read adapters
// need: the wide single-drive read. Declared at the consumer site (CLAUDE.md)
// rather than taking *store.DriveRepo so the REAL adapters can be exercised
// against a scripted drive row — which is how MYR-614's regression is now held
// down at the wiring level rather than only at the handler's stub boundary.
type driveRecordReader interface {
	GetByID(ctx context.Context, id string) (store.DriveRecord, error)
}

// driveAccessFacts projects a store drive row onto the identity BOTH §7.3 and
// §7.4 feed to their access gate.
//
// ONE PRODUCER, FOR THE REASON MYR-614 EXISTS. The route adapter used to build
// its own literal and set two of the three fields — no StartTime — so the
// §7.4 trip-window admission could never parse a start instant and refused
// every participant every route with a 404 that read exactly like a legitimate
// "outside your window". The detail adapter, three lines below it, set the
// field correctly, which is precisely how a divergence like this survives
// review. The two surfaces now cannot disagree about what a drive's access
// identity is, because there is only one place that says.
func driveAccessFacts(rec store.DriveRecord) telemetry.DriveAccessFacts {
	return telemetry.DriveAccessFacts{
		DriveID:   rec.ID,
		VehicleID: rec.VehicleID,
		StartTime: rec.StartTime,
	}
}

// driveRouteAdapter adapts store.DriveRepo.GetByID to the
// telemetry.DriveRouteFetcher interface used by the drive-route handler
// (DV-20). GetByID already resolves the routePointsEnc shadow into the
// decrypted RoutePoints, so the handler sees plaintext. store.ErrDriveNotFound
// wraps sdk.ErrNotFound, so passing the error through with %w lets the
// handler map an unknown drive to a 404.
type driveRouteAdapter struct {
	repo driveRecordReader
}

func (a *driveRouteAdapter) GetDriveRoute(ctx context.Context, driveID string) (telemetry.DriveRouteData, error) {
	rec, err := a.repo.GetByID(ctx, driveID)
	if err != nil {
		return telemetry.DriveRouteData{}, fmt.Errorf("get drive by id: %w", err)
	}
	return telemetry.DriveRouteData{
		DriveAccessFacts: driveAccessFacts(rec),
		RoutePoints:      rec.RoutePoints,
	}, nil
}

// driveDetailAdapter adapts store.DriveRepo.GetByID to the
// telemetry.DriveDetailFetcher interface used by the drive-detail
// handler (MYR-130). GetByID is the wide read — appropriate for a
// detail endpoint — and returns the full DriveRecord; the adapter
// projects it into DriveDetailData, dropping routePoints (served by the
// separate route endpoint). store.ErrDriveNotFound wraps sdk.ErrNotFound,
// so passing the error through with %w lets the handler map an unknown
// drive to a 404.
type driveDetailAdapter struct {
	repo driveRecordReader
}

func (a *driveDetailAdapter) GetDriveDetail(ctx context.Context, driveID string) (telemetry.DriveDetailData, error) {
	rec, err := a.repo.GetByID(ctx, driveID)
	if err != nil {
		return telemetry.DriveDetailData{}, fmt.Errorf("get drive by id: %w", err)
	}
	return telemetry.DriveDetailData{
		DriveAccessFacts: driveAccessFacts(rec),
		EndTime:          rec.EndTime,
		Date:             rec.Date,
		DistanceMiles:    rec.DistanceMiles,
		DurationMinutes:  rec.DurationMinutes,
		AvgSpeedMph:      rec.AvgSpeedMph,
		MaxSpeedMph:      rec.MaxSpeedMph,
		EnergyUsedKwh:    rec.EnergyUsedKwh,
		StartChargeLevel: rec.StartChargeLevel,
		EndChargeLevel:   rec.EndChargeLevel,
		FsdMiles:         rec.FsdMiles,
		FsdPercentage:    rec.FsdPercentage,
		Interventions:    rec.Interventions,
		StartLocation:    rec.StartLocation,
		StartAddress:     rec.StartAddress,
		EndLocation:      rec.EndLocation,
		EndAddress:       rec.EndAddress,
		CreatedAt:        rec.CreatedAt,
	}, nil
}
