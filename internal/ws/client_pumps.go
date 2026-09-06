package ws

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/coder/websocket"
)

// THE TWO PUMPS: one goroutine draining the send channel onto the socket, one
// reading frames off it.
//
// Split from client.go so both stay inside the 300-line cap. The seam is the
// obvious one — that file is the client's STATE and this is its two
// goroutines — and it is worth having: the deadlock the `done` channel exists
// to prevent is a property of these two loops and of nothing else on the
// struct.

// writePump reads messages from the send channel and writes them to the
// WebSocket connection. It exits when the send channel is closed, the
// context is cancelled, or the session is revoked (MYR-373).
//
// The revocation case is not a nicety: a revoked session can never receive
// another message on c.send, so without an out-of-band signal this loop would
// park forever and hold the whole teardown behind it — see Client.done.
func (c *Client) writePump(ctx context.Context, writeTimeout time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			// Revoked. The 4002 close frame is written by RevokeUserAccess
			// directly on the connection, not through this pump, so there is
			// nothing left to flush and nothing to say on the way out.
			return
		case msg, ok := <-c.send:
			if !ok {
				// Hub closed the channel — send a close frame.
				_ = c.conn.Close(websocket.StatusGoingAway, "server shutting down")
				return
			}
			if err := c.writeMessage(ctx, msg, writeTimeout); err != nil {
				c.logger.Debug("write failed, closing client",
					slog.String("user_id", c.userID),
					slog.Any("error", err),
				)
				return
			}
			c.hub.metrics.IncMessagesSent()
		}
	}
}

// readPump reads messages from the WebSocket. After authentication,
// it dispatches client->server control frames (subscribe, unsubscribe,
// ping — DV-07) and ignores any other frame type so unknown messages
// from a future SDK do not poison the connection. Returns when the
// socket is closed or the context cancels.
func (c *Client) readPump(ctx context.Context, writeTimeout time.Duration) {
	c.conn.SetReadLimit(readLimit)
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if !isNormalClose(err) {
				c.logger.Debug("read error",
					slog.String("user_id", c.userID),
					slog.Any("error", err),
				)
			}
			return
		}
		if !c.handleClientFrame(ctx, data, writeTimeout) {
			// Returning false signals a hard close (subscribe to a
			// non-owned vehicle). The handler has already emitted the
			// typed error frame and closed the socket; just exit.
			return
		}
	}
}

// enqueue adds a message to the client's send buffer. If the buffer is
// full, it drops the oldest message to make room (drop-oldest policy).
// Returns true if a message was dropped.
//
// THIS IS THE ONLY WRITER TO c.send, which is what makes the revoked check
// below a real choke point rather than one of several (MYR-373). An earlier
// version of this fix guarded `hasVehicle` instead and was WRONG: the
// snapshot-on-subscribe path (snapshot.go `sendSnapshot`) reaches `enqueue`
// without ever consulting `hasVehicle`, so a revoked session could still pull
// live GPS by sending a `subscribe` frame during the close-handshake window.
// Guarding the channel itself is the version of this claim that cannot be
// bypassed by a path somebody adds later.
//
// A refusal returns FALSE, not true: `true` means "a message was dropped
// because this client is slow" and feeds `IncMessagesDropped`. A revoked
// session is not a slow client, and counting it as one would make a
// revocation look like backpressure on the dashboards.
func (c *Client) enqueue(msg []byte) bool {
	if c.revoked.Load() {
		return false
	}
	select {
	case c.send <- msg:
		return false
	default:
		// Buffer full — drop the oldest message.
		select {
		case <-c.send:
		default:
		}
		// Now try again. This should always succeed because we just
		// drained one slot (or the channel was consumed concurrently).
		select {
		case c.send <- msg:
		default:
			// Extremely unlikely race; just drop the new message.
		}
		return true
	}
}

// hasVehicle reports whether this client is authorized AND currently
// subscribed for the given vehicle ID. allVehicles=true (dev-mode
// NoopAuthenticator) short-circuits to true. Otherwise the vehicleID
// must be in the per-client subscription set, which is initialized
// from vehicleIDs at handshake and modified by subscribe/unsubscribe
// (DV-07 / MYR-46). An empty vehicleIDs slice with allVehicles=false
// means deny-all (NFR-3.21).
//
// A session marked revoked (MYR-373) is deny-all for EVERY vehicle, including
// the dev-mode wildcard, and the check comes first for that reason: it is the
// one condition that must beat every other way of being authorized.
func (c *Client) hasVehicle(vehicleID string) bool {
	if c.revoked.Load() {
		return false
	}
	if c.allVehicles {
		return true
	}
	c.subMu.RLock()
	_, ok := c.subscribed[vehicleID]
	c.subMu.RUnlock()
	return ok
}

// owns reports whether the client was authorized for vehicleID at
// handshake time. Used by the subscribe handler to gate the
// permission_denied path before mutating the subscription set, so the
// ownership check is independent of the current subscription state.
func (c *Client) owns(vehicleID string) bool {
	if c.allVehicles {
		return true
	}
	return slices.Contains(c.vehicleIDs, vehicleID)
}

// subscribe adds vehicleID to the active subscription set. Caller MUST
// have verified ownership (Client.owns) first — the typed error frame
// for vehicle_not_owned is emitted by the readPump dispatcher, not
// here. Idempotent.
func (c *Client) subscribe(vehicleID string) {
	c.subMu.Lock()
	c.subscribed[vehicleID] = struct{}{}
	c.subMu.Unlock()
}

// unsubscribe removes vehicleID from the active subscription set.
// Idempotent: removing an already-absent ID is a no-op. Does NOT
// require ownership — a subscribed-but-since-revoked vehicle should
// still be removable so the client can drain the set on logout.
func (c *Client) unsubscribe(vehicleID string) {
	c.subMu.Lock()
	delete(c.subscribed, vehicleID)
	c.subMu.Unlock()
}

// writeMessage writes a single message to the WebSocket with a timeout.
func (c *Client) writeMessage(ctx context.Context, msg []byte, timeout time.Duration) error {
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := c.conn.Write(writeCtx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("client.writeMessage(user=%s): %w", c.userID, err)
	}
	return nil
}

// isNormalClose reports whether the error represents a normal WebSocket
// closure (client disconnecting cleanly or context cancelled).
func isNormalClose(err error) bool {
	if err == context.Canceled { //nolint:errorlint // exact sentinel match intentional
		return true
	}
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure ||
		status == websocket.StatusGoingAway
}
