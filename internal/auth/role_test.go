package auth

import (
	"errors"
	"testing"
)

func TestRole_String(t *testing.T) {
	tests := []struct {
		name string
		role Role
		want string
	}{
		{name: "owner", role: RoleOwner, want: "owner"},
		{name: "viewer", role: RoleViewer, want: "viewer"},
		{name: "empty sentinel", role: Role(""), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.role.String(); got != tt.want {
				t.Errorf("Role(%q).String() = %q, want %q", tt.role, got, tt.want)
			}
		})
	}
}

func TestParseRole(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Role
		wantErr error
	}{
		{name: "owner", input: "owner", want: RoleOwner},
		{name: "viewer", input: "viewer", want: RoleViewer},
		// THE TWO MYR-602 ROLES, spelled out here as well as reached through
		// TestAllRolesParse. That test proves the two ENUMERATIONS agree with
		// each other; these cases pin the exact wire strings, which is a
		// different fact — a rename that moved both enumerations together
		// would satisfy the first and break every client that has already
		// shipped reading `role`.
		{name: "ride_member", input: "ride_member", want: RoleRideMember},
		{name: "trip_participant", input: "trip_participant", want: RoleTripParticipant},
		{name: "rideMember camelCase rejected", input: "rideMember", wantErr: ErrUnknownRole},
		{name: "tripParticipant camelCase rejected", input: "tripParticipant", wantErr: ErrUnknownRole},
		{name: "empty rejected", input: "", wantErr: ErrUnknownRole},
		{name: "uppercase rejected", input: "Owner", wantErr: ErrUnknownRole},
		{name: "limited_viewer (FR-5.5 future) rejected in v1", input: "limited_viewer", wantErr: ErrUnknownRole},
		{name: "garbage", input: "admin", wantErr: ErrUnknownRole},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRole(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error wrapping %v, got nil", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error wrapping %v, got: %v", tt.wantErr, err)
				}
				if got != Role("") {
					t.Errorf("expected zero-value Role on error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseRole(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestAllRolesParse pins that the two enumerations of the role vocabulary —
// AllRoles' slice and ParseRole's switch — describe the same set.
//
// They are separate on purpose (one is an ordered list, the other a total
// function), and separate enumerations of one set are exactly what drift. A
// role added to only one of them produces either a role no test iterates or a
// role no wire input can name, and neither failure is loud on its own.
func TestAllRolesParse(t *testing.T) {
	seen := make(map[Role]bool, len(AllRoles()))
	for _, role := range AllRoles() {
		if seen[role] {
			t.Errorf("AllRoles() lists %q twice", role)
		}
		seen[role] = true
		parsed, err := ParseRole(string(role))
		if err != nil {
			t.Errorf("ParseRole(%q) = %v — AllRoles names a role ParseRole rejects", role, err)
		}
		if parsed != role {
			t.Errorf("ParseRole(%q) = %q, want %q", role, parsed, role)
		}
	}
	// The other direction: nothing ParseRole accepts may be missing from the
	// list. Checked against the roles this package declares as constants.
	for _, role := range []Role{RoleOwner, RoleViewer, RoleRideMember, RoleTripParticipant} {
		if !seen[role] {
			t.Errorf("%q is a declared role that AllRoles() omits — every for-every-role "+
				"test silently skips it", role)
		}
	}
}

// TestSeesLiveLocationMatchesLiveLocationRoles keeps the predicate and the list
// in agreement. The mask table is built from the list; the audit tests branch on
// the predicate. If they disagree, a role gets the location group from one and
// is asserted not to have it by the other.
func TestSeesLiveLocationMatchesLiveLocationRoles(t *testing.T) {
	inList := make(map[Role]bool, len(LiveLocationRoles()))
	for _, role := range LiveLocationRoles() {
		inList[role] = true
		if role == RoleOwner {
			t.Error("LiveLocationRoles() includes the owner — the list is the NON-OWNER " +
				"exception set, and including the owner would let a narrowing of it " +
				"read as a narrowing of owner access")
		}
	}
	for _, role := range AllRoles() {
		want := inList[role] || role == RoleOwner
		if got := role.SeesLiveLocation(); got != want {
			t.Errorf("%q.SeesLiveLocation() = %v, want %v", role, got, want)
		}
	}
}
