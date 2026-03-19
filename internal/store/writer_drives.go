package store

import (
	"context"
	"log/slog"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// handleDriveStarted returns an event handler that creates a drive record
// when a drive begins.
func (w *Writer) handleDriveStarted(ctx context.Context) events.Handler {
	return func(event events.Event) {
		evt, ok := event.Payload.(events.DriveStartedEvent)
		if !ok {
			w.logger.Error("unexpected payload type for drive.started",
				slog.String("event_id", event.ID),
			)
			return
		}

		vehicleID, err := w.vinCache.resolve(ctx, evt.VIN)
		if err != nil {
			w.logger.Warn("cannot persist drive start: VIN lookup failed",
				slog.String("vin", redactVIN(evt.VIN)),
				slog.String("error", err.Error()),
			)
			return
		}

		record := mapDriveStarted(evt, vehicleID)
		if err := w.drives.Create(ctx, record); err != nil {
			w.logger.Warn("failed to create drive record",
				slog.String("drive_id", evt.DriveID),
				slog.String("vin", redactVIN(evt.VIN)),
				slog.String("error", err.Error()),
			)
		}
	}
}

// handleDriveEnded returns an event handler that completes a drive record,
// appends route points, and sets the vehicle status to parked.
func (w *Writer) handleDriveEnded(ctx context.Context) events.Handler {
	return func(event events.Event) {
		evt, ok := event.Payload.(events.DriveEndedEvent)
		if !ok {
			w.logger.Error("unexpected payload type for drive.ended",
				slog.String("event_id", event.ID),
			)
			return
		}

		completion := mapDriveCompletion(evt)
		if err := w.drives.Complete(ctx, evt.DriveID, completion); err != nil {
			w.logger.Warn("failed to complete drive record",
				slog.String("drive_id", evt.DriveID),
				slog.String("error", err.Error()),
			)
		}

		routePts := mapRoutePoints(evt.Stats.RoutePoints)
		if len(routePts) > 0 {
			if err := w.drives.AppendRoutePoints(ctx, evt.DriveID, routePts); err != nil {
				w.logger.Warn("failed to append route points",
					slog.String("drive_id", evt.DriveID),
					slog.String("error", err.Error()),
				)
			}
		}

		if err := w.vehicles.UpdateStatus(ctx, evt.VIN, VehicleStatusParked); err != nil {
			w.logger.Warn("failed to set vehicle status to parked",
				slog.String("vin", redactVIN(evt.VIN)),
				slog.String("error", err.Error()),
			)
		}
	}
}
