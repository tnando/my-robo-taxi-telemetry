package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupDriveReadEndpoints mounts the two drive-scoped read endpoints —
// GET /api/drives/{driveId}/route (§7.4) and GET /api/drives/{driveId} (§7.3).
//
// Split out of setupHTTPHandlers so that function stays inside the length cap.
// The pair belongs together: both resolve the drive first, take the vehicleId
// off the drive record, and then run the SAME access gate as the drives list —
// owner, or a viewer holding at least `live_history` (MYR-184).
func setupDriveReadEndpoints(deps httpRouteDeps, snapshotAdapter telemetry.VehicleSnapshotReader, trips telemetry.TripDriveAdmitter) {
	deps.srv.HandleFunc("GET /api/drives/{driveId}/route",
		newDriveRouteHandler(deps, snapshotAdapter, trips, &driveRouteAdapter{repo: deps.driveRepo}).ServeHTTP)
	deps.srv.HandleFunc("GET /api/drives/{driveId}",
		newDriveDetailHandler(deps, snapshotAdapter, trips, &driveDetailAdapter{repo: deps.driveRepo}).ServeHTTP)
}

// newDriveRouteHandler builds the §7.4 handler with EVERY production option
// attached.
//
// SPLIT OUT SO A TEST CAN BUILD THE REAL THING (MYR-614). The bug this repairs
// lived in the seam between the adapter and the handler — the handler was
// correct, the adapter was correct in isolation, and the omission only existed
// where they met. A test that hand-rolls its own handler cannot see that seam,
// and the one that hand-rolled its own fetcher never did. The fetcher is the
// one parameter, so a test supplies the REAL adapter over a scripted drive row
// and everything else — the trip admitter, the role resolver, the mask audit —
// is the same object graph production gets.
func newDriveRouteHandler(
	deps httpRouteDeps,
	snapshotAdapter telemetry.VehicleSnapshotReader,
	trips telemetry.TripDriveAdmitter,
	drives telemetry.DriveRouteFetcher,
) *telemetry.DriveRouteHandler {
	// DV-20 (FR-3.3): GET /api/drives/{driveId}/route — the recorded
	// breadcrumb polyline for a completed drive. Same auth + ownership +
	// role-mask flow as the drives list; the route's vehicleId comes off
	// the drive record, then snapshotAdapter checks ownership.
	return telemetry.NewDriveRouteHandler(
		deps.authenticator,
		snapshotAdapter,
		drives,
		deps.logger.With(slog.String("component", "drive-route")),
		telemetry.WithDriveRouteRoleResolver(deps.authenticator),
		// MYR-369: NO share reader. The drives surfaces are owner-only
		// again — the `live_history` capability was removed from the
		// product — and the handler no longer has a seam to pass one
		// through, so re-opening them is a deliberate change rather than
		// one wiring line.
		// MYR-602: a trip participant is admitted to a drive INSIDE one of
		// their windows, and answered 404 — never 403 — for one outside it.
		telemetry.WithDriveRouteTripAdmitter(trips),
		telemetry.WithDriveRouteMaskAudit(deps.auditEmitter, deps.auditMetrics, "/api/drives/{driveId}/route"),
	)
}

// newDriveDetailHandler builds the §7.3 handler with every production option
// attached. Same shape and same reason as newDriveRouteHandler above: the two
// endpoints run the SAME gate over the SAME access facts, so they are worth
// being testable the same way.
func newDriveDetailHandler(
	deps httpRouteDeps,
	snapshotAdapter telemetry.VehicleSnapshotReader,
	trips telemetry.TripDriveAdmitter,
	drives telemetry.DriveDetailFetcher,
) *telemetry.DriveDetailHandler {
	// MYR-130 (FR-3.4): GET /api/drives/{driveId} — the full per-drive
	// stats record (distance, duration, energy, FSD, interventions,
	// start/end loc+addr) minus routePoints. Closes the last DV-20
	// SDK-surface gap. Same auth + ownership + role-mask flow as the
	// route endpoint; the drive's vehicleId comes off the drive record,
	// then snapshotAdapter checks ownership.
	return telemetry.NewDriveDetailHandler(
		deps.authenticator,
		snapshotAdapter,
		drives,
		deps.logger.With(slog.String("component", "drive-detail")),
		telemetry.WithDriveDetailRoleResolver(deps.authenticator),
		// MYR-369: NO share reader. The drives surfaces are owner-only
		// again — the `live_history` capability was removed from the
		// product — and the handler no longer has a seam to pass one
		// through, so re-opening them is a deliberate change rather than
		// one wiring line.
		// MYR-602: same window admission as the route endpoint above — the two
		// take the SAME admitter, so a drive a participant can open cannot be
		// one whose route they are refused.
		telemetry.WithDriveDetailTripAdmitter(trips),
		telemetry.WithDriveDetailMaskAudit(deps.auditEmitter, deps.auditMetrics, "/api/drives/{driveId}"),
	)
}
