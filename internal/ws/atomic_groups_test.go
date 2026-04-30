package ws

import "testing"

func TestGroupOf(t *testing.T) {
	tests := []struct {
		field     string
		wantGroup atomicGroupID
		wantOK    bool
	}{
		// Navigation members.
		{"routeLine", groupNavigation, true},
		{"destinationName", groupNavigation, true},
		{"minutesToArrival", groupNavigation, true},
		{"milesToArrival", groupNavigation, true},
		{"destinationLocation", groupNavigation, true},
		{"originLocation", groupNavigation, true},

		// Charge members.
		{"soc", groupCharge, true},
		{"chargeState", groupCharge, true},
		{"estimatedRange", groupCharge, true},
		{"timeToFull", groupCharge, true},

		// GPS members.
		{"location", groupGPS, true},
		{"heading", groupGPS, true},

		// Gear member.
		{"gear", groupGear, true},

		// Individual fields (no atomic group).
		{"speed", "", false},
		{"odometer", "", false},
		{"insideTemp", "", false},
		{"hvacPower", "", false},
		{"fsdMilesSinceReset", "", false},

		// Unknown field.
		{"nonexistent", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			gotGroup, gotOK := groupOf(tt.field)
			if gotGroup != tt.wantGroup || gotOK != tt.wantOK {
				t.Fatalf("groupOf(%q) = (%q, %v), want (%q, %v)",
					tt.field, gotGroup, gotOK, tt.wantGroup, tt.wantOK)
			}
		})
	}
}

// TestAtomicGroupMembersDisjoint asserts that no internal field name is
// declared in more than one atomic group. The groupOf() iteration order
// over the map is non-deterministic; two-group membership would produce
// flaky behavior at runtime.
func TestAtomicGroupMembersDisjoint(t *testing.T) {
	seen := make(map[string]atomicGroupID)
	for groupID, members := range atomicGroupMembers {
		for field := range members {
			if other, dup := seen[field]; dup {
				t.Fatalf("field %q appears in both %q and %q",
					field, other, groupID)
			}
			seen[field] = groupID
		}
	}
}
