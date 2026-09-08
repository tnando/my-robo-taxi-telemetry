package drives

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

const energyEpsilon = 1e-9

func nearly(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > energyEpsilon {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestDriveEnergy_Accumulates covers the accumulator in isolation: what it
// credits, what it refuses, and what it reports when it has nothing.
func TestDriveEnergy_Accumulates(t *testing.T) {
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	type sample struct {
		kwh         float64
		afterSecs   int
		chargeState string
	}

	tests := []struct {
		name         string
		seed         float64
		samples      []sample
		wantKwh      float64
		wantReported bool
		wantCharged  bool
	}{
		{
			name:         "no samples at all reports nothing",
			wantReported: false,
		},
		{
			name:         "a baseline alone is not a measurement",
			seed:         60,
			wantReported: false,
		},
		{
			name:         "one in-drive sample against the seeded baseline measures",
			seed:         60,
			samples:      []sample{{kwh: 57.5, afterSecs: 30}},
			wantKwh:      2.5,
			wantReported: true,
		},
		{
			name: "consumption sums across samples",
			seed: 60,
			samples: []sample{
				{kwh: 58, afterSecs: 30},
				{kwh: 55, afterSecs: 60},
				{kwh: 51, afterSecs: 90},
			},
			wantKwh:      9,
			wantReported: true,
		},
		{
			name: "a self-seeding drive measures from its first sample",
			samples: []sample{
				{kwh: 60, afterSecs: 30},
				{kwh: 55, afterSecs: 60},
			},
			wantKwh:      5,
			wantReported: true,
		},
		{
			name: "regen inside the rate bound is credited as negative consumption",
			seed: 60,
			samples: []sample{
				{kwh: 55, afterSecs: 30},
				// +0.5 kWh over 30s = 60 kW, under the 70 kW ceiling.
				{kwh: 55.5, afterSecs: 60},
			},
			wantKwh:      4.5,
			wantReported: true,
		},
		{
			name: "a gain beyond the regen rate bound is charging and is excluded",
			seed: 60,
			samples: []sample{
				{kwh: 40, afterSecs: 1800},
				// +15 kWh: no regen returns that, whatever the interval.
				{kwh: 55, afterSecs: 3600},
				{kwh: 50, afterSecs: 3630},
			},
			wantKwh:      25, // 20 driving before the charge + 5 after it
			wantReported: true,
			wantCharged:  true,
		},
		{
			name: "the car saying Charging excludes even a small gain",
			seed: 60,
			samples: []sample{
				{kwh: 55, afterSecs: 30},
				{kwh: 55.4, afterSecs: 60, chargeState: "Charging"},
				{kwh: 54, afterSecs: 90},
			},
			wantKwh:      6.4, // 5 + 1.4; the 0.4 gain is dropped, baseline rebased
			wantReported: true,
			wantCharged:  true,
		},
		{
			name: "Complete is not charging so the gain falls to the rate bound",
			seed: 60,
			samples: []sample{
				{kwh: 60.2, afterSecs: 30, chargeState: "Complete"},
			},
			wantKwh:      -0.2,
			wantReported: true,
		},
		{
			name: "a drive spent entirely charging reports nothing",
			seed: 20,
			samples: []sample{
				{kwh: 40, afterSecs: 600, chargeState: "Charging"},
				{kwh: 60, afterSecs: 1200, chargeState: "Charging"},
			},
			wantReported: false,
			wantCharged:  true,
		},
		{
			name:         "zero readings are rejected, never differenced",
			seed:         60,
			samples:      []sample{{kwh: 0, afterSecs: 30}, {kwh: 58, afterSecs: 60}},
			wantKwh:      2,
			wantReported: true,
		},
		{
			name:         "a zero seed does not establish a baseline",
			seed:         0,
			samples:      []sample{{kwh: 60, afterSecs: 30}, {kwh: 58, afterSecs: 60}},
			wantKwh:      2,
			wantReported: true,
		},
		{
			name:         "a stationary sample is a credited step of zero",
			seed:         60,
			samples:      []sample{{kwh: 60, afterSecs: 30}},
			wantKwh:      0,
			wantReported: true,
		},
		{
			// The deliberate asymmetry named in observe(): a gain is bounded by
			// what regen could return, a LOSS is not bounded at all. Whatever
			// the pack lost across a two-hour hole in the stream — real driving,
			// vampire drain, a pack-level re-estimate — is credited to the drive
			// that was open across it.
			name: "a large loss over a long gap is credited in full, unbounded",
			seed: 60,
			samples: []sample{
				{kwh: 5, afterSecs: 7200},
			},
			wantKwh:      55,
			wantReported: true,
		},
		{
			name: "a gain over a non-positive interval is charging, never regen",
			seed: 60,
			samples: []sample{
				{kwh: 60.1, afterSecs: 0},
			},
			wantReported: false,
			wantCharged:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e driveEnergy
			if tt.seed != 0 {
				e.seed(tt.seed, base)
			}
			for _, s := range tt.samples {
				e.observe(s.kwh, base.Add(time.Duration(s.afterSecs)*time.Second), s.chargeState)
			}

			got, reported := e.total()
			if reported != tt.wantReported {
				t.Fatalf("reported = %v, want %v (kwh=%v)", reported, tt.wantReported, got)
			}
			if reported {
				// The table states the PHYSICAL figure, negatives included: a
				// net-regen drive reports its negative and the floor lives on
				// the window total, not on the drive (review finding 3).
				nearly(t, got, tt.wantKwh, "total")
			}
			if e.chargedInside != tt.wantCharged {
				t.Errorf("chargedInside = %v, want %v", e.chargedInside, tt.wantCharged)
			}
		})
	}
}

// TestDriveEnergy_NetRegenDriveReportsItsNegative pins the review round's
// correction (finding 3). A long descent really can end with more charge than
// it began with, and that is a MEASUREMENT — the physical figure is negative
// and the accumulator reports it.
//
// The clamp this replaces manufactured a 0, which is the exact value
// `Trip.totalEnergyKwh` reads as "this drive was never measured": a real
// measurement of a downhill leg voided the whole window's total, the clamp
// defeating the rule it existed to protect. The floor now sits on the window
// sum (GREATEST in queryTripDriveTotals), where a negative leg still cannot
// cancel another leg's consumption.
func TestDriveEnergy_NetRegenDriveReportsItsNegative(t *testing.T) {
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var e driveEnergy
	e.seed(50, base)
	e.observe(50.5, base.Add(30*time.Second), "")
	e.observe(51, base.Add(60*time.Second), "")

	nearly(t, e.usedKwh, -1, "raw accumulator")

	got, reported := e.total()
	if !reported {
		t.Fatal("reported = false, want true")
	}
	nearly(t, got, -1, "total")
}

// ── THE REGRESSION THIS ISSUE EXISTS FOR ────────────────────────────────────

func floatField(v float64) events.TelemetryValue {
	return events.TelemetryValue{FloatVal: ptr(v)}
}

func stringField(v string) events.TelemetryValue {
	return events.TelemetryValue{StringVal: ptr(v)}
}

// energyDetector builds a Detector wired to a real bus but never started — the
// energy tests drive handleTelemetry directly so they observe the in-memory
// activeDrive rather than racing the debounce timer.
func energyDetector() *Detector {
	d := NewDetector(testBus(), testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	d.ctx = context.Background()
	return d
}

// activeDriveFor returns the vehicle's in-progress drive, failing the test if
// there is none.
func activeDriveFor(t *testing.T, d *Detector, vin string) *activeDrive {
	t.Helper()
	val, ok := d.states.Load(vin)
	if !ok {
		t.Fatalf("no state for vin %s", vin)
	}
	state := val.(*vehicleState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.drive == nil {
		t.Fatalf("no active drive for vin %s (status=%s)", vin, state.status)
	}
	return state.drive
}

// TestDetector_EnergyFromIdleCacheWhenStartFrameLacksIt is the MYR-629
// regression in one test. The gear-change frame that opens a drive carries NO
// energy — EnergyRemaining streams at 30s against gear's 1s — and before this
// issue that made `startEnergy` zero, which the old `if startEnergy == 0`
// guard turned into `energyUsedKwh = 0` for 453 of the last 460 production
// drives. The cached idle reading is the correct baseline.
func TestDetector_EnergyFromIdleCacheWhenStartFrameLacksIt(t *testing.T) {
	d := energyDetector()

	vin := "TESTVIN000ENERGY1"
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	// Parked: energy and odometer stream while the car sits still.
	d.handleTelemetry(telemetryEvent(vin, base, map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            stringField("P"),
		string(telemetry.FieldEnergyRemaining): floatField(60),
		string(telemetry.FieldOdometer):        floatField(1000),
	}))

	// The gear frame that opens the drive carries gear and nothing else.
	d.handleTelemetry(telemetryEvent(vin, base.Add(time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldGear): stringField("D"),
	}))

	// One in-drive energy sample.
	d.handleTelemetry(telemetryEvent(vin, base.Add(31*time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            stringField("D"),
		string(telemetry.FieldEnergyRemaining): floatField(57.4),
		string(telemetry.FieldOdometer):        floatField(1010),
	}))

	drive := activeDriveFor(t, d, vin)
	got, reported := drive.energy.total()
	if !reported {
		t.Fatal("energy not reported; the idle cache did not seed the baseline")
	}
	nearly(t, got, 2.6, "energy")
}

// TestDetector_EnergySelfSeedsWhenNothingWasCached covers the cold-start half:
// no idle sample was ever seen (a server that came up mid-drive), so the
// accumulator seeds itself from the first in-drive reading and reports the
// consumption after it rather than reporting nothing.
func TestDetector_EnergySelfSeedsWhenNothingWasCached(t *testing.T) {
	d := energyDetector()

	vin := "TESTVIN000ENERGY2"
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	d.handleTelemetry(telemetryEvent(vin, base, map[string]events.TelemetryValue{
		string(telemetry.FieldGear): stringField("D"),
	}))
	d.handleTelemetry(telemetryEvent(vin, base.Add(time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldGear): stringField("D"),
	}))
	d.handleTelemetry(telemetryEvent(vin, base.Add(31*time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldEnergyRemaining): floatField(50),
	}))
	d.handleTelemetry(telemetryEvent(vin, base.Add(61*time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldEnergyRemaining): floatField(47),
	}))

	drive := activeDriveFor(t, d, vin)
	got, reported := drive.energy.total()
	if !reported {
		t.Fatal("energy not reported")
	}
	nearly(t, got, 3, "energy")
}

// TestDetector_ChargingInsideAnOpenDriveIsExcluded is the second half of the
// issue's ask. A drive stays open across a stop shorter than EndDebounce and
// while the watchdog waits out a car that parked without sending gear=P — which
// is how a Supercharger stop lands inside a road-trip leg. The energy the cable
// put in must not cancel the energy the road took out.
func TestDetector_ChargingInsideAnOpenDriveIsExcluded(t *testing.T) {
	d := energyDetector()

	vin := "TESTVIN000ENERGY3"
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	d.handleTelemetry(telemetryEvent(vin, base, map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            stringField("P"),
		string(telemetry.FieldEnergyRemaining): floatField(70),
	}))
	d.handleTelemetry(telemetryEvent(vin, base.Add(time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldGear): stringField("D"),
	}))
	// Drove it down to 20 kWh.
	d.handleTelemetry(telemetryEvent(vin, base.Add(30*time.Minute), map[string]events.TelemetryValue{
		string(telemetry.FieldEnergyRemaining): floatField(20),
	}))
	// Charged back to 65 with the drive still open.
	d.handleTelemetry(telemetryEvent(vin, base.Add(50*time.Minute), map[string]events.TelemetryValue{
		string(telemetry.FieldEnergyRemaining): floatField(65),
		string(telemetry.FieldChargeState):     stringField("Charging"),
	}))
	// Drove another 10 kWh worth.
	d.handleTelemetry(telemetryEvent(vin, base.Add(80*time.Minute), map[string]events.TelemetryValue{
		string(telemetry.FieldEnergyRemaining): floatField(55),
	}))

	drive := activeDriveFor(t, d, vin)
	got, reported := drive.energy.total()
	if !reported {
		t.Fatal("energy not reported")
	}
	// 50 driven before the charge + 10 after it. A start/end subtraction would
	// have said 15.
	nearly(t, got, 60, "energy")
	if !drive.energy.chargedInside {
		t.Error("chargedInside = false, want true")
	}
}

// TestCalculateStats_EnergyDeltaComesFromTheAccumulator pins the wiring: the
// figure the store persists as `energyUsedKwh` is the accumulator's total and
// nothing else.
func TestCalculateStats_EnergyDeltaComesFromTheAccumulator(t *testing.T) {
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	t.Run("measured drive carries its figure", func(t *testing.T) {
		drive := &activeDrive{startedAt: base, lastTimestamp: base.Add(time.Hour)}
		drive.energy.seed(60, base)
		drive.energy.observe(48, base.Add(time.Hour), "")

		nearly(t, calculateStats(drive).EnergyDelta, 12, "EnergyDelta")
	})

	t.Run("unmeasurable drive carries zero, not a guess", func(t *testing.T) {
		drive := &activeDrive{startedAt: base, lastTimestamp: base.Add(time.Hour)}
		drive.energy.seed(60, base)

		nearly(t, calculateStats(drive).EnergyDelta, 0, "EnergyDelta")
	})
}

// TestDriveEnergy_GainAtExactlyTheAllowanceIsRegen pins the STRICT `>` in
// observe's charging test, from both sides of the boundary.
//
// The numbers are chosen so the boundary is REACHABLE rather than approximated:
// 70 kW across 45 seconds is 0.875 kWh, which is exact in binary floating point,
// and 10.0 + 0.875 is exact too — so the gain observe() computes is bit-for-bit
// the allowance regenAllowance() returns, and the comparison is genuinely the
// equality case rather than something a rounding error either side of it.
func TestDriveEnergy_GainAtExactlyTheAllowanceIsRegen(t *testing.T) {
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	at := base.Add(45 * time.Second)

	var e driveEnergy
	e.seed(10, base)
	if got := e.regenAllowance(at); got != 0.875 {
		t.Fatalf("regenAllowance = %v, want exactly 0.875 — the fixture's exactness depends on it", got)
	}

	// Exactly the allowance: credited, because the test is `-step > allowance`.
	e.observe(10.875, at, "")
	got, reported := e.total()
	if !reported {
		t.Fatal("a gain of exactly the allowance was not credited at all")
	}
	nearly(t, got, -0.875, "total")
	if e.chargedInside {
		t.Error("chargedInside = true; a gain AT the allowance is regen, not charging")
	}

	// One ulp more is charging: dropped, and the baseline rebased.
	var over driveEnergy
	over.seed(10, base)
	over.observe(math.Nextafter(10.875, math.Inf(1)), at, "")
	if _, reported := over.total(); reported {
		t.Error("a gain one ulp beyond the allowance was credited; the bound is not strict")
	}
	if !over.chargedInside {
		t.Error("chargedInside = false for a gain beyond the allowance")
	}
}

// ── THE CADENCE MISMATCH THE REVIEW ROUND FOUND ─────────────────────────────
//
// `EnergyRemaining` is configured at IntervalSeconds: 30 and
// `DetailedChargeState` is on-change with a 120s resend, so at best ONE energy
// frame in FOUR carries a charge state. These two tests drive a charging
// session at exactly that ratio.
//
// chargingSessionTotal opens a drive on a car whose parked energy is cached,
// burns `burnKwh` on one in-drive sample, then charges at `kw` for 15 minutes
// in 30s steps with `chargeState: Charging` on every FOURTH frame — the shape
// the stream actually delivers. It returns the drive's accumulator.
func chargingSessionTotal(t *testing.T, vin string, burnKwh, kw float64) *driveEnergy {
	t.Helper()
	d := energyDetector()
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	// Parked with a known pack level, then the gear frame that opens the drive.
	d.handleTelemetry(telemetryEvent(vin, base, map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            stringField("P"),
		string(telemetry.FieldEnergyRemaining): floatField(60),
	}))
	d.handleTelemetry(telemetryEvent(vin, base.Add(time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldGear): stringField("D"),
	}))

	// One real driving sample: the drive burned `burnKwh` getting to the plug.
	level := 60 - burnKwh
	d.handleTelemetry(telemetryEvent(vin, base.Add(30*time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldEnergyRemaining): floatField(level),
	}))

	// The car parks at a charger without ever sending gear=P, so the drive is
	// still open (the stall watchdog is what eventually closes it). 15 minutes
	// of charging at 30s steps.
	perStep := kw * 30.0 / 3600.0
	for i := range 30 {
		at := base.Add(time.Duration(60+30*i) * time.Second)
		level += perStep
		fields := map[string]events.TelemetryValue{
			string(telemetry.FieldEnergyRemaining): floatField(level),
		}
		// Every fourth frame carries the charge state; the other three do not.
		if i%4 == 0 {
			fields[string(telemetry.FieldChargeState)] = stringField("Charging")
		}
		d.handleTelemetry(telemetryEvent(vin, at, fields))
	}

	return &activeDriveFor(t, d, vin).energy
}

// TestDetector_DCChargingInsideDriveCreditsNoRegen is the case the rate bound
// already caught on its own: 150 kW puts 1.25 kWh into the pack every 30s, far
// beyond what regenerative braking could return, so every step is refused
// whether or not the frame said "Charging".
func TestDetector_DCChargingInsideDriveCreditsNoRegen(t *testing.T) {
	e := chargingSessionTotal(t, "TESTVIN000ENERGY4", 5, 150)

	got, reported := e.total()
	if !reported {
		t.Fatal("energy not reported; the driving sample before the plug should have measured")
	}
	nearly(t, got, 5, "total")
	if !e.chargedInside {
		t.Error("chargedInside = false, want true")
	}
}

// TestDetector_ACChargingInsideDriveCreditsNoRegen is the one the rate bound
// CANNOT catch, and the reason the charge state is latched. 11 kW is 0.092 kWh
// per 30s step — comfortably inside the ~0.58 kWh a 70 kW regen ceiling admits
// over the same interval — so the bound credits every step as regen and the
// drive under-reports what it burned.
//
// ⚠ THIS TEST FAILS WITHOUT THE LATCH. Reading `chargeState` off the frame that
// carried the energy sample answers on one step in four; the other three are
// credited as regen and the total lands near 2.9 kWh instead of 5.
func TestDetector_ACChargingInsideDriveCreditsNoRegen(t *testing.T) {
	e := chargingSessionTotal(t, "TESTVIN000ENERGY5", 5, 11)

	got, reported := e.total()
	if !reported {
		t.Fatal("energy not reported; the driving sample before the plug should have measured")
	}
	nearly(t, got, 5, "total")
	if !e.chargedInside {
		t.Error("chargedInside = false, want true")
	}
}
