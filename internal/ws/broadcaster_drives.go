package ws

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// THE TWO DRIVE FRAMES, and the only two on this surface that are ROLE-SPLIT.
//
// Split from broadcaster.go so both stay inside the 300-line cap, and along a
// seam worth having: every other handler in that file fans out through the
// role-BLIND Hub.Broadcast, while both of these carry something a narrowed
// `viewer` is not entitled to — a raw start coordinate, and a summary of how
// far and how fast somebody drove. Keeping them together is what makes the two
// redactions readable as one rule rather than as two special cases.

// handleDriveStarted transforms a DriveStartedEvent into a drive_started
// message and broadcasts it.
func (b *Broadcaster) handleDriveStarted(ctx context.Context, event events.Event) {
	payload, ok := event.Payload.(events.DriveStartedEvent)
	if !ok {
		b.logger.Error("broadcaster.handleDriveStarted: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	vehicleID, err := b.resolver.GetByVIN(ctx, payload.VIN)
	if err != nil {
		b.logger.Warn("broadcaster.handleDriveStarted: VIN resolution failed, skipping event",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	frame := driveStartedPayload{
		VehicleID: vehicleID,
		DriveID:   payload.DriveID,
		StartLocation: startLocation{
			Latitude:  payload.Location.Latitude,
			Longitude: payload.Location.Longitude,
		},
		Timestamp: payload.StartedAt.Format(time.RFC3339),
	}
	msg, err := marshalWSMessage(msgTypeDriveStarted, frame)
	if err != nil {
		b.logger.Error("broadcaster.handleDriveStarted: marshal failed",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	// MYR-602 — `startLocation` IS THE CAR'S RAW POSITION, and this frame used
	// to go out through the role-blind Hub.Broadcast. A plain `viewer` no
	// longer receives the Speed/GPS group on any other surface, so leaving this
	// one unmasked would have handed them the single most locating coordinate
	// the car emits: where its owner was standing when they set off, twice a
	// day, indefinitely.
	//
	// The EVENT still reaches them — a viewer sees the car's status flip to
	// driving anyway, and withholding it would make their drive list disagree
	// with the owner's about whether anything happened. Only the coordinate is
	// replaced, with the documented (0,0) no-fix sentinel, which every consumer
	// already handles because a car that has never reported a position emits
	// it. See hub_location_frames.go.
	redactedFrame := frame
	redactedFrame.StartLocation = redactedStartLocation()
	redacted, err := marshalWSMessage(msgTypeDriveStarted, redactedFrame)
	if err != nil {
		// The located frame is fine; only the redaction failed. Sending the
		// located one to everybody would be the leak this exists to close, so
		// the viewers get nothing this time and the owner still gets their
		// frame — fail closed, and say so.
		b.logger.Error("broadcaster.handleDriveStarted: redacted marshal failed; viewers withheld",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
	}

	b.hub.BroadcastByLocationAccess(vehicleID, msg, redacted)
}

// handleDriveEnded transforms a DriveEndedEvent into a drive_ended
// message and broadcasts it. It also flushes any remaining accumulated
// route points and clears the accumulator for the vehicle.
func (b *Broadcaster) handleDriveEnded(ctx context.Context, event events.Event) {
	payload, ok := event.Payload.(events.DriveEndedEvent)
	if !ok {
		b.logger.Error("broadcaster.handleDriveEnded: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	vehicleID, err := b.resolver.GetByVIN(ctx, payload.VIN)
	if err != nil {
		b.logger.Warn("broadcaster.handleDriveEnded: VIN resolution failed, skipping event",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	// Flush any remaining route points before sending drive_ended.
	if remaining := b.routes.Flush(payload.VIN); len(remaining) > 0 {
		b.broadcastRoutePoints(ctx, event.ID, payload.VIN, remaining)
	}
	b.routes.Clear(payload.VIN)

	// Flush any pending nav fields for this VIN. Flush cancels the timer
	// and clears state, so a separate Clear call is unnecessary.
	if navFields := b.groups.Flush(groupNavigation, payload.VIN); len(navFields) > 0 {
		b.flushGroup(groupNavigation, payload.VIN, navFields)
	}

	ended := driveEndedPayload{
		VehicleID:       vehicleID,
		DriveID:         payload.DriveID,
		Distance:        payload.Stats.Distance,
		DurationSeconds: payload.Stats.Duration.Seconds(),
		AvgSpeed:        payload.Stats.AvgSpeed,
		MaxSpeed:        payload.Stats.MaxSpeed,
		Timestamp:       payload.EndedAt.Format(time.RFC3339),
	}
	msg, err := marshalWSMessage(msgTypeDriveEnded, ended)
	if err != nil {
		b.logger.Error("broadcaster.handleDriveEnded: marshal failed",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	// MYR-602 — THE SUMMARY IS DRIVE DATA, and this frame went out role-blind
	// alongside drive_started. Since the narrowing, a plain `viewer` gets no
	// drives at all: §7.2's list is owner-and-participant, and §7.30.7 is what
	// a trip ADDS. Yet every viewer subscribed to the car was still told, twice
	// a day, exactly how far it went, for how long, and how fast it was driven
	// — a behavioural record of somebody's driving assembled from a stream they
	// were not supposed to be reading.
	//
	// THE EVENT ITSELF STAYS, for drive_started's reason: a viewer already sees
	// the car's `status` flip out of driving, so suppressing the frame would
	// only make the two surfaces disagree about whether anything happened. Only
	// the four numbers are withheld — and they are withheld by ZEROING rather
	// than by dropping, because all four are in DriveEndedPayload's `required`
	// list under `additionalProperties: false`. Removing them does not narrow
	// the frame; it makes the document undecodable for every installed build,
	// which is the same collision the vehicle_state sentinels resolve the same
	// way (internal/mask/sentinels.go).
	//
	// A CONSUMER CANNOT TELL "withheld" FROM "a drive of no distance" BY
	// READING THE VALUE — they are the same bytes by design — and MUST branch
	// on the role it holds, exactly as rest-api.md §5 already requires of it
	// for the location sentinels.
	redacted, err := marshalWSMessage(msgTypeDriveEnded, redactedDriveEnded(ended))
	if err != nil {
		// The full frame is fine; only the redaction failed. Sending the full
		// one to everybody would be the leak this exists to close, so the
		// viewers get nothing this time and the owner still gets their frame —
		// fail closed, and say so. Same posture as drive_started's.
		b.logger.Error("broadcaster.handleDriveEnded: redacted marshal failed; viewers withheld",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
	}

	b.hub.BroadcastByLocationAccess(vehicleID, msg, redacted)
}
