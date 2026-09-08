package store

import "errors"

// The shapes MYR-609 added to the sharing surface: the three refusals §7.5.8
// extend owes that no other path has, and the tombstone-authorship vocabulary
// migration 0051 introduced. Split from vehicle_share_types.go under the
// 300-line rule.
//
// ALL THREE SENTINELS MAP TO 409 conflict WITH NO SUB-CODE, and the absence is
// the design rather than an omission. `already_shared` — the one sub-code on
// this endpoint — exists to tell a client the call is a SUCCESS to render,
// which is what makes an "Add all" affordance safe. These three are the
// opposite: nothing happened, and each names a different thing the owner must
// do first. A client that branched on a sub-code here would be branching on
// "try again after doing something else", which is the message's job.

var (
	// ErrShareSourceSuspended is returned by ExtendShare when the grant
	// being extended is PAUSED (MYR-609). The HTTP layer maps it to 409
	// conflict with NO sub-code.
	//
	// NO SUB-CODE, DELIBERATELY, and it is not an oversight beside
	// `already_shared`. That sub-code exists to tell a client the call is a
	// SUCCESS to render — the person already has the car — which is what
	// makes an "Add all" affordance safe. This one is the opposite: nothing
	// happened, and the remedy is an owner action on another screen. A
	// client that branched on a sub-code here would be branching on "try
	// again after doing something else", which is the message's job.
	//
	// The earlier cut copied the pause forward instead of refusing. That
	// wrote a grant born suspended — invisible to the grantee, shown as
	// shared on the owner's own screen, and needing a §7.5.7 PATCH nobody
	// would know to make.
	ErrShareSourceSuspended = errors.New("source vehicle share is suspended")

	// ErrShareTargetSuspended is returned by ExtendShare when the grantee
	// already holds a PAUSED grant on the target vehicle (MYR-609). The
	// HTTP layer maps it to 409 conflict with NO sub-code.
	//
	// EXPLICITLY NOT ErrShareAlreadyGranted. A paused grant conveys nothing
	// (the §7.5.0 suspension invariant), so answering `already_shared` —
	// which the contract tells clients to render as success — would report
	// "they already have this car" about somebody who currently has no
	// access to it at all, and would leave the owner believing a pause they
	// set is not in their way when it is the only thing that is.
	ErrShareTargetSuspended = errors.New("vehicle share on the target vehicle is suspended")

	// ErrShareGranteeLeft is returned by ExtendShare when the newest
	// tombstone for (target vehicle, grantee) was written by the GRANTEE —
	// they used the §7.5.7 leave (MYR-609, migration 0051 `revoked_by`).
	// The HTTP layer maps it to 409 conflict with NO sub-code.
	//
	// Leaving is the ONE exit a grantee has from a share. Extending onto the
	// car they left would hand the access straight back with no act by them
	// and no notification to them, which makes the exit reversible by the
	// party they were leaving. The owner's route is a fresh invite the
	// person can decline by not redeeming it — i.e. the consent this
	// endpoint is otherwise entitled to assume, asked for again.
	//
	// An OWNER-authored tombstone does not produce this: an owner
	// re-sharing a car they themselves un-shared is the ordinary case. A
	// tombstone with no recorded author (written before migration 0051) does
	// not produce it either — see the migration for why unknown fails open.
	ErrShareGranteeLeft = errors.New("grantee left the target vehicle")
)

// Tombstone AUTHORS — the `revoked_by` vocabulary (migration 0051, MYR-609).
// A revoked row records not just THAT access ended but WHO ended it, because
// §7.5.8 extend must not undo a grantee's own §7.5.7 leave.
//
// NULL is a third state and is not spelled here on purpose: it means the author
// was never recorded (any tombstone predating migration 0051), it is not
// writable by anything in this package, and it fails OPEN at the extend gate.
const (
	// ShareRevokedByOwner covers every write the owner's side makes: the
	// §7.5.3 revoke, the vehicle-offboarding sweep, and the redeem path's
	// SUPERSEDED tombstone.
	ShareRevokedByOwner = "owner"
	// ShareRevokedByGrantee covers the §7.5.7 leave and the grantee's own
	// account deletion. It is the only value that blocks an extend.
	ShareRevokedByGrantee = "grantee"
)

// ShareRevokedReasonSuperseded is the `revoked_reason` written when the redeem
// path retires a PENDING row it cannot accept: the redeemer already holds a
// live accepted grant on that car through another invite, so this row can never
// become one and would otherwise sit pending until it expired, backing a code
// its siblings already consumed. The tombstone is authored by the OWNER
// (ShareRevokedByOwner) because it is a consequence of how the owner composed
// the invite, not an act of the person redeeming it — and so it does not block
// a later extend.
const ShareRevokedReasonSuperseded = "superseded"
