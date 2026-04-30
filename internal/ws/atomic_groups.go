package ws

// atomicGroupID identifies a v1 atomic field group declared in
// docs/contracts/vehicle-state-schema.md §1.1, §2, and §3.
//
// Fields within the same group are emitted together (or held) so partial
// groups never reach SDK clients (NFR-3.3, NFR-3.4). Per-group atomicity
// is guaranteed by one of three mechanisms:
//
//   - groupNavigation: server-side groupAccumulator with a 500 ms flush
//     window. Navigation siblings can arrive across multiple Tesla frames
//     within one second, so the accumulator merges them into a single
//     vehicle_update.
//   - groupCharge: Tesla's 500 ms upstream bucket. When charge siblings
//     change in the same vehicle-side bucket they arrive in one Payload
//     protobuf message (vehicle-state-schema.md §2.2). No server-side
//     accumulator.
//   - groupGPS, groupGear: synchronous emission. lat/lng co-emit from one
//     Tesla Location proto; gear's status is derived from gearPosition at
//     broadcast time. No accumulator needed.
type atomicGroupID string

const (
	groupNavigation atomicGroupID = "navigation"
	groupCharge     atomicGroupID = "charge"
	groupGPS        atomicGroupID = "gps"
	groupGear       atomicGroupID = "gear"
)

// atomicGroupMembers maps each atomic group to the set of internal
// telemetry field names contributing to it. Internal field names are the
// names produced by internal/telemetry; wire-side translation happens
// downstream in mapFieldsForClient.
//
// Source of truth for group membership; the WS broadcaster partitions
// telemetry events through groupOf() against this map. Keep in sync with
// docs/contracts/vehicle-state-schema.md §1.1.
var atomicGroupMembers = map[atomicGroupID]map[string]struct{}{
	groupNavigation: {
		"routeLine":           {},
		"destinationName":     {},
		"minutesToArrival":    {},
		"milesToArrival":      {},
		"destinationLocation": {},
		"originLocation":      {},
	},
	groupCharge: {
		"soc":            {}, // wire: chargeLevel
		"chargeState":    {},
		"estimatedRange": {},
		"timeToFull":     {},
	},
	groupGPS: {
		"location": {}, // wire: latitude + longitude (split server-side)
		"heading":  {},
	},
	groupGear: {
		"gear": {}, // wire: gearPosition; status is derived
	},
}

// groupOf returns the atomicGroupID for an internal telemetry field name.
// The bool is false when the field belongs to no atomic group — those
// fields are individual values broadcast immediately on arrival
// (vehicle-state-schema.md §1.1 "Individual fields (no atomic group)").
func groupOf(internalFieldName string) (atomicGroupID, bool) {
	for id, members := range atomicGroupMembers {
		if _, ok := members[internalFieldName]; ok {
			return id, true
		}
	}
	return "", false
}
