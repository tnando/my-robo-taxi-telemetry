package push

import (
	"context"
	"errors"
	"log/slog"
)

// ONE DEVICE, and what Apple's answer means about it.
//
// Split from notifier_send.go so both stay inside the 300-line cap, along the
// seam the two halves already had: that file decides WHETHER and TO WHOM a
// notification goes — the preference gate, the island gate, the device lookup —
// while this one is the single place a Notification is actually assembled and
// the single place a 410 removes a registration. Building a notification in two
// places is how one of them stops carrying a header, which is precisely the
// MYR-554 class of bug.

// send delivers to one device and applies the APNs feedback: a permanently
// rejected token is removed from the registry so the next ride does not retry
// a phone that no longer exists. Reports whether the send succeeded.
func (n *Notifier) send(ctx context.Context, dev Device, d delivery, a alert) bool {
	notification := Notification{
		DeviceToken: dev.Token,
		Sandbox:     dev.Sandbox,
		Title:       a.title,
		Body:        a.body,
		RideID:      d.rideID,
		// MYR-554: the (subject, topic) pair the collapse id is built from. It
		// is the fan-out's OWN topic string — the same one every log line here
		// carries — so the id names the notification's intent and nothing about
		// the attempt that carries it.
		EventTopic: d.topic,
	}
	// MYR-602: a trips push names no ride, so it carries its own userInfo and
	// its own collapse subject. Applied here, at the one place a Notification
	// is built, rather than at the trips fan-out site — everything else about
	// the delivery (the preference gate, the 410 correction, the log
	// discipline) is shared, and building the notification in two places is how
	// one of them stops carrying a header.
	if d.tripPush != nil {
		notification.UserInfo = d.tripPush.userInfo()
		notification.CollapseSubject = d.tripPush.collapseSubject()
	}

	err := n.sender.Send(ctx, notification)
	if err == nil {
		return true
	}

	if errors.Is(err, ErrUnregistered) {
		n.dropDevice(ctx, dev.Token, d.topic)
		return false
	}

	n.logger.Warn("push: send failed",
		slog.String("topic", d.topic),
		slog.String("ride_id", d.rideID),
		slog.String("trip_id", d.tripID()),
		slog.String("device_token_prefix", tokenPrefix(dev.Token)),
		slog.String("error", err.Error()),
	)
	return false
}

// tripID renders the trip this delivery is about for a log line, or "" when it
// is an ordinary ride push. Both ids are P0 opaque cuids.
func (d delivery) tripID() string {
	if d.tripPush == nil {
		return ""
	}
	return d.tripPush.TripID
}

// dropDevice removes a token APNs reported as permanently dead. The delete
// runs on a context DETACHED from the fan-out's, which may already be at its
// deadline — precisely when the registry most needs the correction to land.
func (n *Notifier) dropDevice(ctx context.Context, deviceToken, topic string) {
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	if err := n.stores.devices.DeleteDeviceToken(delCtx, deviceToken); err != nil {
		n.logger.Error("push: failed to delete unregistered device",
			slog.String("topic", topic),
			slog.String("device_token_prefix", tokenPrefix(deviceToken)),
			slog.String("error", err.Error()),
		)
		return
	}
	n.logger.Info("push: deleted unregistered device",
		slog.String("topic", topic),
		slog.String("device_token_prefix", tokenPrefix(deviceToken)),
	)
}

// vehicleName resolves a vehicle nickname for the copy, best-effort. A failure
// is logged at debug and yields "", which the copy renders as a generic label
// — a notification with a slightly blander title beats no notification.
// requesterFirstName resolves the rider's first name for owner-facing copy,
// or "" when unwired, unknown or unreadable — the copy's anonymous fallback
// handles all three the same way. The value is P1 and is never logged.
func (n *Notifier) requesterFirstName(ctx context.Context, userID string) string {
	if n.requesters == nil || userID == "" {
		return ""
	}
	name, err := n.requesters.RequesterFirstName(ctx, userID)
	if err != nil {
		n.logger.Debug("push: requester name lookup failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return ""
	}
	return name
}

func (n *Notifier) vehicleName(ctx context.Context, vehicleID string) string {
	if n.vehicles == nil || vehicleID == "" {
		return ""
	}
	name, err := n.vehicles.VehicleName(ctx, vehicleID)
	if err != nil {
		n.logger.Debug("push: vehicle name lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		return ""
	}
	return name
}
