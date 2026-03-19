package store

import (
	"math"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// mapTelemetryToUpdate converts a map of telemetry field values into a
// VehicleUpdate with only the present fields set. Fields not recognized
// or missing from the map are left nil (no-op on the database update).
func mapTelemetryToUpdate(fields map[string]events.TelemetryValue) *VehicleUpdate {
	u := &VehicleUpdate{}
	hasFields := false

	for name, val := range fields {
		switch telemetry.FieldName(name) {
		case telemetry.FieldSpeed:
			if v := floatToIntPtr(val.FloatVal); v != nil {
				u.Speed = v
				hasFields = true
			}
		case telemetry.FieldLocation:
			if val.LocationVal != nil {
				u.Latitude = &val.LocationVal.Latitude
				u.Longitude = &val.LocationVal.Longitude
				hasFields = true
			}
		case telemetry.FieldHeading:
			if v := floatToIntPtr(val.FloatVal); v != nil {
				u.Heading = v
				hasFields = true
			}
		case telemetry.FieldGear:
			if val.StringVal != nil {
				u.GearPosition = val.StringVal
				hasFields = true
			}
		case telemetry.FieldSOC, telemetry.FieldBatteryLevel:
			if v := floatToIntPtr(val.FloatVal); v != nil {
				u.ChargeLevel = v
				hasFields = true
			}
		case telemetry.FieldEstBatteryRange:
			if v := floatToIntPtr(val.FloatVal); v != nil {
				u.EstimatedRange = v
				hasFields = true
			}
		case telemetry.FieldInsideTemp:
			if v := floatToIntPtr(val.FloatVal); v != nil {
				u.InteriorTemp = v
				hasFields = true
			}
		case telemetry.FieldOutsideTemp:
			if v := floatToIntPtr(val.FloatVal); v != nil {
				u.ExteriorTemp = v
				hasFields = true
			}
		case telemetry.FieldOdometer:
			if v := floatToIntPtr(val.FloatVal); v != nil {
				u.OdometerMiles = v
				hasFields = true
			}
		}
	}

	if !hasFields {
		return nil
	}
	return u
}

// floatToIntPtr rounds a float64 to the nearest int and returns a pointer.
// Returns nil if the input pointer is nil.
func floatToIntPtr(f *float64) *int {
	if f == nil {
		return nil
	}
	v := int(math.Round(*f))
	return &v
}
