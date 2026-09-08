package drives

// The per-vehicle field caches, split out of state.go so both files stay under
// the 300-line cap (CLAUDE.md "File Rules"). state.go declares the state; this
// file is what fills it in from a telemetry frame.

import (
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// latchedChargeState returns the last charge state the vehicle reported, or ""
// when it has never reported one. "" is the honest answer for a car that has
// never charged since we started listening: isChargingState("") is false, so
// the energy accumulator falls through to its rate bound rather than guessing.
// The caller must hold s.mu.
func (s *vehicleState) latchedChargeState() string {
	if !s.chargeStateKnown {
		return ""
	}
	return s.lastChargeState
}

// cacheFrameFields folds one telemetry frame into the per-vehicle caches. The
// caller must hold s.mu.
//
// GEAR AND LOCATION ARE HERE BECAUSE THE STATE MACHINE READS THEM ON EVERY
// FRAME. The five that follow — FSD miles, odometer, SOC, energy and the charge
// state — are ONE FAMILY WITH ONE REASON, which is why they now live in one
// function rather than scattered down handleTelemetry. `Gear` is configured at
// IntervalSeconds: 1 while they stream at 30s (the charge group and energy) or
// 60s (the odometer and the FSD counter), and Tesla emits a field only when
// BOTH its interval has elapsed AND its value changed. So the gear-change frame
// that OPENS a drive essentially never carries any of them, and a drive that
// read its baselines off that frame read zeros.
//
// That defect has been found four times: SOC (MYR-207, the "0%% -> 75%%, -75%%
// used" summary), the odometer and the FSD counter (MYR-157), and energy
// (MYR-629, `energyUsedKwh` a hard 0 on 453 of the last 460 production drives).
// The fix is the same one every time — cache the last value seen INCLUDING
// WHILE PARKED, because none of these move in a parked car, so the last idle
// sample IS the correct drive-start baseline — and gathering them here is what
// makes the fifth instance of the pattern obvious to whoever adds the next
// slow-cadence field.
//
// The `known` flags are not decoration: a cache holding a zero it was never
// told is exactly the state that produced MYR-207, so every consumer asks
// whether a value was ever observed rather than testing it against zero.
func (s *vehicleState) cacheFrameFields(te events.VehicleTelemetryEvent) {
	// Extract gear from the telemetry fields (may be absent).
	gear := extractStringField(te.Fields, telemetry.FieldGear)
	if gear != "" {
		s.lastGear = gear
	}

	// Cache location if present for use when drives start without GPS.
	if loc := extractLocation(te.Fields); loc != nil {
		s.lastLocation = loc
	}

	// Cache FSD miles whenever present (including while idle) so startDrive
	// can seed the drive baseline from the most recent value — the
	// gear-change event that starts a drive almost never carries FSD miles
	// because it streams on a much slower cadence than gear.
	if fsd, ok := extractFloatField(te.Fields, telemetry.FieldFSDMiles); ok {
		s.lastFSDMiles = fsd
		s.fsdMilesKnown = true
	}

	// Cache odometer the same way (MYR-157) so startDrive can seed an
	// accurate distance baseline — odometer streams on the same slow
	// cadence as FSD and is usually absent from the gear-change frame.
	if odo, ok := extractFloatField(te.Fields, telemetry.FieldOdometer); ok {
		s.lastOdometer = odo
		s.odometerKnown = true
	}

	// Cache SOC the same way (MYR-207) so startDrive can seed
	// startChargeLevel from the last-known charge — the gear-change frame
	// that starts a drive frequently lacks the charge atomic group, which
	// otherwise persisted startChargeLevel=0. Only cache non-zero values:
	// a literal 0 is exactly the zero-value default we are guarding
	// against, never a plausible parked charge.
	if soc, ok := extractFloatField(te.Fields, telemetry.FieldSOC); ok && soc > 0 {
		s.lastSOC = soc
		s.socKnown = true
	}

	// Cache energyRemaining the same way (MYR-629) so startDrive can seed the
	// consumption baseline from the last-known pack level. EnergyRemaining is
	// the last member of the slow-cadence family (SOC, odometer, FSD miles)
	// that never got this treatment, and its absence is why `energyUsedKwh`
	// read 0 on 453 of the last 460 production drives. Only positive values:
	// a literal 0 is the zero-value default, never a plausible pack level.
	if energy, ok := extractFloatField(te.Fields, telemetry.FieldEnergyRemaining); ok && energy > 0 {
		s.lastEnergy = energy
		s.energyKnown = true
	}

	// Latch the charge state the same way (MYR-629 review round), and note
	// this one is a STRING cache with no plausibility filter beyond
	// non-emptiness: "" means the frame said nothing, every other value is
	// something the car asserted. It is latched rather than read per frame
	// because `chargeState` and `energyRemaining` stream on different cadences
	// and only about one energy frame in four carries a charge state — see the
	// field comment in state.go.
	if cs := extractStringField(te.Fields, telemetry.FieldChargeState); cs != "" {
		s.lastChargeState = cs
		s.chargeStateKnown = true
	}
}
