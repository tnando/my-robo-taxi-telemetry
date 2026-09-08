package ws

import (
	"log/slog"
)

// Mid-connection access WIDENING for share grants (MYR-609, closing the
// residual half of websocket-protocol.md §10 DV-09).
//
// hub_access.go next door explains why a NARROWING closes the session rather
// than editing Client.vehicleIDs in place: the protocol defines no per-vehicle
// membership frame, vehicleIDs is read lock-free by every broadcast, and the
// reconnect re-derives the access set and the per-vehicle roles together in the
// one place that already does it correctly. Every one of those arguments holds
// here unchanged, and DV-09's own residual note prescribes exactly this — "if a
// widening producer ever ships, extend the same event with a re-handshake
// reason rather than mutating Client.vehicleRoles in place".
//
// TWO THINGS DIFFER FROM THE NARROWING, and both fall out of the direction.
//
//  1. THE SESSIONS CANNOT BE FOUND BY THE VEHICLE. RevokeUserAccess filters the
//     user's sessions down to the ones authorized for the affected car. A
//     grantee who just GAINED a car is, by definition, not yet authorized for
//     it — that is the whole bug — so the same filter would match nothing and
//     the widening would silently do nothing. Every session this user holds is
//     re-handshaked instead.
//
//  2. THE FRAME IS THE SAME 4002, DELIBERATELY. It is not a lie about what
//     happened, because the frame does not describe what happened: §6.2 defines
//     4002's client contract as "reconnect, then render whatever the new
//     handshake returns", with an explicit instruction NOT to auto-retry a
//     subscribe for the vehicle that was open. That is precisely the correct
//     behavior here, and it is behavior every deployed SDK already has. A new
//     4xxx code would be a WIRE CHANGE that buys nothing: no client would do
//     anything different, and one that did not recognize it would be worse off.
//     The reason string is byte-identical too, so a widening and a narrowing are
//     indistinguishable on the wire — which also means a viewer is never told
//     the difference between "you lost a car" and "you gained one" by the close
//     alone, and finds out by reconnecting, which is the only honest way for a
//     client to learn either.
//
// The cost is that a grantee is briefly disconnected from everything to be told
// about something they gained. That is the same trade every other access change
// on this hub makes, it is invisible behind the SDK's reconnect, and the
// alternative — the car missing from their map for the life of the session — is
// the bug this exists to fix.

// WidenUserAccess makes every session belonging to userID re-handshake, so an
// access set that GREW is picked up now rather than at the client's next
// reconnect (MYR-609).
//
// Implemented as RevokeUserAccess over ALL of the user's sessions — see the
// file comment for why the vehicle cannot be used to narrow the set, and why
// the same close frame is the right one. vehicleID is carried for the log only.
//
// An empty userID is a no-op, NOT a wildcard. Safe to call for a user with no
// live sessions (the common case), and idempotent.
//
// RETURNS AS SOON AS THE SESSIONS ARE CUT OFF, not when their TCP connections
// finish dying — the same split RevokeUserAccess documents, for the same reason:
// a graceful close waits up to five seconds for a peer that may never answer,
// and this is called from a single per-subscription bus goroutine.
func (h *Hub) WidenUserAccess(userID, vehicleID, reason string) int {
	if userID == "" {
		return 0
	}

	// The empty vehicleID is the "every session this user holds" form, which
	// is the one a widening needs. It is passed explicitly rather than by
	// omission so a reader of this line can see the choice being made.
	closed := h.RevokeUserAccess(userID, "", reason)

	h.logger.Info("re-handshaking client: share access widened",
		slog.String("user_id", userID),
		slog.String("vehicle_id", vehicleID),
		slog.String("reason", reason),
		slog.Int("sessions_closed", closed),
	)
	return closed
}
