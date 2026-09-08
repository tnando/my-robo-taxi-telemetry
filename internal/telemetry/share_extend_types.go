package telemetry

import "errors"

// The shapes §7.5.8 extend adds to the sharing surface (MYR-609): its request
// input, the three refusals no other share route has, and the WIDENING
// counterpart to ShareAccessNotifier.
//
// EXPORTED for the same reason the sentinels in share_invite_types.go are: the
// adapter in cmd/telemetry-server translates the equivalent internal/store
// values into them at the boundary, because internal/telemetry does not import
// internal/store.

// ShareExtendInput is the validated extend request as it crosses into the
// store dependency.
type ShareExtendInput struct {
	OwnerUserID string
	// TargetVehicleID is the PATH vehicle — the car gaining the grant.
	TargetVehicleID string
	// SourceShareID is the accepted grant being extended.
	SourceShareID string
}

// The three refusals that are unique to extend. All map to 409 conflict WITH NO
// SUB-CODE, and the absence is the design.
//
// `already_shared` — the one sub-code this endpoint emits — exists to tell a
// client the call is a SUCCESS to render: the person already has the car, so an
// "Add all" affordance can treat it as done and move on. These three are the
// opposite. Nothing happened, and each names a different thing the owner has to
// do somewhere else first. A sub-code here would be a machine-readable "try
// again after doing something", which is the message's job and not a branch a
// client can act on generically.
var (
	// ErrShareSourceSuspended reports that the grant being extended is
	// PAUSED. → 409 conflict, no sub-code.
	//
	// The pause is not copied forward. Copying it would write a grant born
	// suspended: invisible to the grantee, shown as shared on the owner's
	// own screen, and needing a §7.5.7 PATCH nobody would know to make.
	ErrShareSourceSuspended = errors.New("source share is suspended")

	// ErrShareTargetSuspended reports that the grantee already holds a
	// PAUSED grant on the path vehicle. → 409 conflict, no sub-code.
	//
	// EXPLICITLY NOT ErrShareAlreadyGranted. A paused grant conveys nothing,
	// so `already_shared` — which the contract tells clients to render as
	// success — would report "they already have this car" about somebody who
	// currently has no access to it, and would leave the owner believing
	// their own pause is not the thing in the way.
	ErrShareTargetSuspended = errors.New("share on the target vehicle is suspended")

	// ErrShareGranteeLeft reports that the grantee LEFT the path vehicle
	// under §7.5.7. → 409 conflict, no sub-code.
	//
	// Leaving is the one exit a grantee has. Re-granting the car on the
	// owner's button press, with no act by the grantee and no notification
	// to them, makes that exit reversible by the party they were leaving.
	// The owner's route is a fresh invite, which the person can decline by
	// not redeeming it.
	ErrShareGranteeLeft = errors.New("grantee left the target vehicle")
)

// ShareAccessWidener announces that a grantee has GAINED access to a vehicle,
// so a WebSocket they already hold picks the car up now instead of at their
// next reconnect (MYR-609, websocket-protocol.md §4.5.1 / §10 DV-09).
//
// IT IS THE MIRROR OF ShareAccessNotifier AND IT EXISTS FOR THE SAME REASON.
// `Client.vehicleIDs` is frozen at handshake; the access CACHE is what the next
// handshake consults, and a socket that already completed one consults nothing
// again. So a widening needs two signals exactly as a narrowing does: one to
// fix the next connection (AccessCacheInvalidator) and one to fix the current
// one (this).
//
// DV-09 recorded the widening direction as a known, benign residual — "shows a
// client LESS than it is entitled to, never more" — with no live producer,
// because with `live_history` retired the only editable flag was `allowRides`,
// which has no WebSocket effect. §7.5.8 is the first real producer, and the
// same note prescribes this shape: "extend the same event with a re-handshake
// reason rather than mutating Client.vehicleRoles in place", because the
// broadcast fan-out reads that map lock-free and reconnect already re-derives
// the access set and the roles together in the one place that does it
// correctly. MYR-601 (viewer→owner transfer) is the same class and should use
// this rather than inventing a second mechanism.
//
// Defined at the consumer site and deliberately primitive-valued so this
// package does not take a dependency on internal/events, exactly like
// ShareAccessNotifier above it.
//
// Optional, like the invalidator and the notifier: a nil widener leaves the
// pre-MYR-609 behavior in place — the grantee's REST surface sees the car
// immediately and their open socket picks it up at its next reconnect or at
// the 60-second revalidation sweep.
//
// MUST NOT BLOCK. It is called on the request path with the owner waiting on
// their 201; implementations publish to an in-process bus and return.
type ShareAccessWidener interface {
	// ShareAccessWidened announces that granteeUserID has GAINED access to
	// vehicleID. reason is for the server log only.
	ShareAccessWidened(granteeUserID, vehicleID, reason string)
}
