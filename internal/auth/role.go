package auth

import (
	"errors"
	"fmt"
)

// Role identifies the access level a caller has against a particular
// vehicle. Roles are resolved per (user, vehicle) at WebSocket handshake
// time and at REST request time and feed the field-mask layer in
// internal/mask. See docs/contracts/rest-api.md §5 for the canonical
// matrix and FR-5.4 / FR-5.5 for the v1 vs. future-role contract.
type Role string

const (
	// RoleOwner identifies the user who owns the vehicle on the Prisma
	// "Vehicle" table (Vehicle.userId == caller). Owners see every
	// SDK-exposed field.
	RoleOwner Role = "owner"

	// RoleViewer identifies a user who has been invited to view a
	// vehicle they do not own. Viewers receive the full real-time stream
	// minus the fields enumerated as owner-only in rest-api.md §5.2:
	// the full `vin` on the snapshot (MYR-279) and the owner-curated
	// `name` on the vehicles list. `licensePlate` is deliberately NOT
	// owner-only (MYR-286) — riders need it to identify the car.
	RoleViewer Role = "viewer"

	// RoleRideMember identifies a caller who is RIDING in a vehicle right now
	// on a live ride they requested or joined (MYR-540). Introduced by MYR-602,
	// which split it out of RoleViewer.
	//
	// Before MYR-602 a riding member resolved to RoleViewer and inherited the
	// viewer allow-list, which then carried the whole location and navigation
	// groups. MYR-602 NARROWED RoleViewer (client decision: a standing share
	// must not carry live location), and splitting this role out is what keeps
	// ride tracking working unchanged: RoleRideMember's field set is exactly
	// the pre-MYR-602 viewer set.
	//
	// Ride-scoped and self-expiring. It is not a standing grant, there is
	// nothing to revoke, and it ends when the ride reaches a terminal status —
	// the status predicate in the access query IS the expiry.
	RoleRideMember Role = "ride_member"

	// RoleTripParticipant identifies a caller inside the ACTIVE WINDOW of a
	// trip they are a live participant of (MYR-602).
	//
	// Same field set as RoleRideMember, and that is a deliberate identity
	// rather than a coincidence: both roles mean "this person is party to
	// where the car is going RIGHT NOW", and giving them one shared list is
	// what stops the two drifting into a distinction nobody intended. The
	// roles stay separate anyway because they carry different PROVENANCE —
	// which is what decides drive access (a trip participant may read the
	// window's drives; a ride member may not read any) and what a future
	// narrowing would key on.
	//
	// Window-scoped. It ends at the window edge with nothing to revoke, and
	// it can never outlive the accepted share the participant was picked
	// from — the access query re-joins that share on every resolution.
	RoleTripParticipant Role = "trip_participant"
)

// AllRoles enumerates every role this package can resolve, strongest first.
//
// Stated once, here, so a test that must hold FOR EVERY ROLE — the wire-role
// projection, the mask-audit sweep, the conformance harness — iterates a list
// that grows with the vocabulary instead of a literal somebody has to remember
// to extend. ParseRole's switch is the other enumeration of the same set, and
// TestAllRolesParse pins that the two agree.
func AllRoles() []Role {
	return []Role{RoleOwner, RoleTripParticipant, RoleRideMember, RoleViewer}
}

// LiveLocationRoles enumerates the non-owner roles that receive the location
// and navigation groups. Stated as a list rather than left implicit in the mask
// table so the security property has ONE place to read: exactly these roles,
// and RoleViewer is not among them.
func LiveLocationRoles() []Role { return []Role{RoleRideMember, RoleTripParticipant} }

// SeesLiveLocation reports whether a role receives the location/navigation
// groups. Owners always do; among non-owners only the two window-scoped roles
// do. Used by the mask-audit tests and by the contract-conformance harness.
func (r Role) SeesLiveLocation() bool {
	switch r {
	case RoleOwner, RoleRideMember, RoleTripParticipant:
		return true
	default:
		return false
	}
}

// ErrUnknownRole is returned by ParseRole when the input is not one of
// the known roles. Treat the empty Role("") sentinel separately — it is the
// fail-closed "unknown" value the mask layer interprets as deny-all.
var ErrUnknownRole = errors.New("unknown role")

// String implements fmt.Stringer.
func (r Role) String() string {
	return string(r)
}

// ParseRole validates a string against the v1 role enum. The empty
// string is intentionally rejected: the empty Role("") value is used as
// a fail-closed "unknown" sentinel inside the mask layer (deny-all
// projection) and MUST NOT be produced by parsing user input. See
// rest-api.md §5 for the fail-closed semantics.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOwner, RoleViewer, RoleRideMember, RoleTripParticipant:
		return Role(s), nil
	default:
		return Role(""), fmt.Errorf("auth.ParseRole(%q): %w", s, ErrUnknownRole)
	}
}
