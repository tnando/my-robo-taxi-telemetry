package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// The MYR-609 share-WIDENING nudge, wired end to end at cmd/ so neither the
// REST handler nor the hub has to know the other exists (the dependency rule in
// CLAUDE.md). The exact mirror of share_access_dispatcher.go next door, in two
// halves:
//
//	shareWidenBusNotifier  — REST side. §7.5.8 extend calls it the moment the
//	                         grant commits; it publishes
//	                         events.ShareAccessWidenedEvent and returns.
//	shareWidenDispatcher   — hub side. Subscribed to that topic; makes the
//	                         grantee's live sessions re-handshake.
//
// A SEPARATE FILE AND A SEPARATE TOPIC rather than a `reason` on the revocation
// pipeline, even though the hub call underneath is nearly the same. The two are
// opposite in the property that matters everywhere else: a revocation is a
// SECURITY action whose latency is a live GPS leak, a widening is a convenience
// whose worst outcome is a car appearing a reconnect later. Anything that
// watches, counts or alerts on revocations must not have widenings folded into
// its numbers, and a future change to either one must not have to reason about
// the other.

// shareWidenBusNotifier satisfies telemetry.ShareAccessWidener by publishing
// onto the in-process bus.
type shareWidenBusNotifier struct {
	bus    events.Bus
	logger *slog.Logger
}

func newShareWidenBusNotifier(bus events.Bus, logger *slog.Logger) *shareWidenBusNotifier {
	return &shareWidenBusNotifier{bus: bus, logger: logger}
}

// ShareAccessWidened publishes the widening. It is called ON THE REQUEST PATH
// with the owner waiting on their 201, so it must not block: the bus fan-out is
// a non-blocking send onto a buffered per-subscriber channel.
//
// A publish failure is LOGGED, NOT RETURNED, and does not fail the owner's
// request — the same posture the revocation notifier takes, and an easier call
// here. The grant has committed and the cache has been busted, so the access
// really is granted and every REST surface already says so; what a lost publish
// costs is that the grantee's already-open socket picks the car up at its next
// reconnect or at the 60-second revalidation sweep instead of immediately.
// Failing the owner's extend over that would report a share that worked as one
// that did not.
func (n *shareWidenBusNotifier) ShareAccessWidened(granteeUserID, vehicleID, reason string) {
	if n.bus == nil || granteeUserID == "" {
		return
	}
	evt := events.NewEvent(events.ShareAccessWidenedEvent{
		GranteeUserID: granteeUserID,
		VehicleID:     vehicleID,
		Reason:        reason,
	})
	if err := n.bus.Publish(context.Background(), evt); err != nil {
		n.logger.Error("share access widening not published; live socket will pick the car up at reconnect",
			slog.String("user_id", granteeUserID),
			slog.String("vehicle_id", vehicleID),
			slog.String("reason", reason),
			slog.Any("error", err),
		)
	}
}

// shareWidenDispatcher re-handshakes a grantee's live WebSocket sessions when
// their access set grows.
type shareWidenDispatcher struct {
	hub    *ws.Hub
	logger *slog.Logger
}

func newShareWidenDispatcher(hub *ws.Hub, logger *slog.Logger) *shareWidenDispatcher {
	return &shareWidenDispatcher{hub: hub, logger: logger}
}

// Subscribe registers the dispatcher on events.TopicShareAccessWidened.
func (d *shareWidenDispatcher) Subscribe(bus events.Bus) (events.Subscription, error) {
	return bus.Subscribe(events.TopicShareAccessWidened, d.handle)
}

// handle re-handshakes the grantee's sessions. Everything it does is
// idempotent, so a duplicate event costs one extra reconnect at most.
func (d *shareWidenDispatcher) handle(evt events.Event) {
	payload, ok := evt.Payload.(events.ShareAccessWidenedEvent)
	if !ok {
		d.logger.Warn("share_access_widened dispatcher: wrong payload type",
			slog.String("topic", string(evt.Topic)),
		)
		return
	}
	if d.hub == nil || payload.GranteeUserID == "" {
		return
	}

	closed := d.hub.WidenUserAccess(payload.GranteeUserID, payload.VehicleID, payload.Reason)
	d.logger.Info("dispatched share access widening",
		slog.String("user_id", payload.GranteeUserID),
		slog.String("vehicle_id", payload.VehicleID),
		slog.String("reason", payload.Reason),
		slog.Int("sessions_closed", closed),
	)
}
