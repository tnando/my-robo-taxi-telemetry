package events

import "time"

// RawVehicleTelemetryEvent mirrors VehicleTelemetryEvent but preserves the
// full set of decoded protobuf fields keyed by proto field number — including
// fields that the broadcast pipeline filters out via the InternalFieldName
// lookup. It exists to power the dev-only debug WebSocket endpoint and the
// cmd/ops CLI inspector; production traffic never emits this topic.
type RawVehicleTelemetryEvent struct {
	BasePayload
	VIN       string
	CreatedAt time.Time
	Fields    []RawTelemetryField
}

// EventTopic returns TopicVehicleTelemetryRaw.
func (RawVehicleTelemetryEvent) EventTopic() Topic { return TopicVehicleTelemetryRaw }

// RawTelemetryField carries one decoded Tesla datum with its proto field
// metadata preserved. The Value holds the concrete Go type chosen by the
// decoder for that field (float64, int64, string, bool, *Location, or nil).
type RawTelemetryField struct {
	ProtoField int32  // Tesla proto field number (e.g. 43 for TimeToFullCharge)
	ProtoName  string // Tesla Field enum name (e.g. "TimeToFullCharge")
	Type       string // Go type tag: "float", "int", "string", "bool", "location", "invalid"
	Value      any    // Raw value; nil when Invalid is true
	Invalid    bool   // True when the vehicle marked the datum invalid
}
