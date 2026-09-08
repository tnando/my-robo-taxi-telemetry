package telemetry

// THE SECOND HALF OF EVERY ACCESS-GROWING MUTATION IN THIS PACKAGE, in one
// place (MYR-601 review round).
//
// Four endpoints publish a widening — §7.5.8 extend, §7.5.5 redeem, an
// un-suspending §7.5.4 PATCH and the §7.24 ride join — and each had grown its
// own copy of the same four lines on its own handler type. The copies were
// byte-identical apart from the receiver, which is the shape a rule takes just
// before one copy drifts: a nil check quietly dropped in one of them would be a
// nil-pointer panic on a live request, and a missing empty-user check would
// reach the hub as a WILDCARD and disconnect every client on the process.
//
// A free function rather than a shared embedded struct, because the seam is a
// value each handler already holds and the handlers have nothing else in
// common. The narrowing direction keeps its own spelling on ShareInviteHandler
// (endLiveAccess) — there is exactly one of it, and the two directions are
// deliberately not interchangeable.

// publishAccessWidened announces that granteeUserID's access set now contains
// vehicleID, so their already-open sessions re-handshake and pick it up.
//
// ORDER IS THE CALLER'S RESPONSIBILITY AND IT IS LOAD-BEARING: every call site
// busts that user's cached access set FIRST. The publish provokes a reconnect,
// and a handshake served from the pre-mutation cache would come back WITHOUT
// the car — a no-op that looks like a fix.
//
// Best-effort by construction. A nil widener (dev mode, or a deployment with no
// bus), an empty grantee id, or a grantee who is simply not connected are all
// ordinary no-ops: the mutation has already committed and no user-facing
// response depends on anybody being online to hear about it. The empty id is
// refused HERE rather than at the hub because it is not a wildcard, and the one
// place that must never be in doubt is the one every producer funnels through.
func publishAccessWidened(widener ShareAccessWidener, granteeUserID, vehicleID, reason string) {
	if widener == nil || granteeUserID == "" {
		return
	}
	widener.ShareAccessWidened(granteeUserID, vehicleID, reason)
}
