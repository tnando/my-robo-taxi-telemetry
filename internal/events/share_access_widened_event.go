package events

// ShareAccessWidenedEvent is published when an owner action GROWS ONE
// grantee's access to ONE vehicle: today only a rest-api.md §7.5.8 extend
// (MYR-609). It is the widening mirror of ShareAccessRevokedEvent and it
// closes the residual half of websocket-protocol.md §10 DV-09.
//
// WHY A WIDENING NEEDS AN EVENT AT ALL. The WS access set is frozen on the
// Client at handshake, so it is stale in BOTH directions, not just the
// dangerous one. DV-09 recorded the widening direction as benign — it shows a
// client less than it is entitled to, never more — and true at the time,
// because nothing produced one. §7.5.8 does: an owner extends a car onto
// somebody who is connected, the owner is told it worked, the grantee's REST
// surface shows the car, and their live map does not have it until they happen
// to reconnect. Benign is not the same as invisible.
//
// Published by the sharing handler AFTER the grant has committed and AFTER the
// grantee's cached access set has been busted, in that order. The ordering is
// load-bearing for the mirror-image reason the revoked event's is: the consumer
// makes the client reconnect, the reconnect re-runs the handshake against
// GetUserVehicles, and a handshake served from the pre-mutation cache would
// come back WITHOUT the car — a no-op that looks like a fix.
//
// SCOPED TO THE GRANTEE, never to the vehicle at large. Nobody else's sessions
// are touched; the owner's own stream is not interrupted to tell somebody else
// about a car they gained.
type ShareAccessWidenedEvent struct {
	BasePayload

	// GranteeUserID is the person who GAINED access. Required; an empty
	// value is dropped by the consumer rather than being read as "everyone".
	GranteeUserID string

	// VehicleID is the car they gained. Carried for the log and for future
	// consumers; the hub does not filter on it, because a client that is
	// not yet authorized for the new car cannot be found BY that car — the
	// whole reason a widening cannot reuse the revocation's vehicle-scoped
	// session lookup.
	VehicleID string

	// Reason is the owner action that caused this, for the log only:
	// "extended". It never reaches the wire.
	Reason string
}

// EventTopic returns TopicShareAccessWidened.
func (ShareAccessWidenedEvent) EventTopic() Topic { return TopicShareAccessWidened }
