package drives

import "time"

// ── PER-DRIVE ENERGY FROM THE STREAM (MYR-629) ──────────────────────────────
//
// `Drive.energyUsedKwh` existed since the first drive-tracker and was WRONG in
// production for the whole of that time. The prod read that opened MYR-629:
// over the last 30 days, 7 of 460 drive rows carried a positive
// `energyUsedKwh` (441 of those drives actually moved), and in September the
// figure was 0 of 96. The column is `NOT NULL`, so the failure never showed as
// a null — it showed as a hard zero on a 77-mile drive.
//
// WHY. The old computation was `startEnergy - lastEnergy`, where `startEnergy`
// was read ONLY from the gear-change frame that opened the drive, and then
// zeroed the whole delta when that read came up empty:
//
//	energyDelta := drive.startEnergy - drive.lastEnergy
//	if drive.startEnergy == 0 { energyDelta = 0 }
//
// `Gear` streams at IntervalSeconds: 1 and `EnergyRemaining` at 30, and Tesla
// emits a field only when BOTH the interval has elapsed AND the value moved —
// so the frame that flips the car into D essentially never carries energy, and
// the guard then discarded every drive. This is EXACTLY the MYR-207 defect
// ("0% -> 75%, -75% used"), which was fixed for SOC and then again for the
// odometer and the FSD counter (MYR-157) by caching the last idle value on the
// vehicle and lazily seeding from the first in-drive sample. Energy was the one
// member of that family the fix never reached.
//
// WHAT REPLACES IT is this accumulator: a RUNNING SUM of per-sample deltas
// rather than a two-point subtraction. Two reasons, and the second is the one
// the issue asked for.
//
//   - A sum survives a missing endpoint. Any two consecutive energy readings
//     inside the drive produce a real figure, so a drive that never saw a
//     baseline before it began still reports what it burned after its first
//     sample instead of reporting nothing.
//   - A sum can EXCLUDE an interval. A two-point subtraction cannot tell a
//     drive that consumed 20 kWh from a drive that consumed 20 kWh and then
//     charged 15 back; it reports 5. The accumulator drops the charging step
//     and keeps the 20.
//
// CHARGING INSIDE A DRIVE IS REAL, not a hypothetical. A drive stays open
// across a stop shorter than `EndDebounce` (30s), and it stays open while the
// watchdog waits out `StallTimeout` on a car that parked without ever sending
// gear=P — which is the ordinary way a Supercharger stop lands in the middle of
// a road-trip leg.
//
// TELLING CHARGING FROM REGEN, the one judgement call here. Both raise
// `EnergyRemaining`. Regen is not an error and MUST be credited: rolling down a
// pass genuinely puts charge back and a drive's honest consumption is the net.
// Charging is not consumption at all and must be excluded. The discriminator is
// two-part, and the cheap half is authoritative:
//
//  1. THE CAR SAYS SO. `chargeState` (proto 179 DetailedChargeState) reads
//     "Charging" or "Starting" while the pack is taking power from a cable. When
//     the frame carries it, it decides, and no rate arithmetic is consulted.
//  2. A RATE BOUND, for the frames that do not. `chargeState` is emitted on
//     change with a 120s resend, so a gain can arrive on a frame that says
//     nothing about charging. A gain is credited as regen only while it is
//     within what regenerative braking could physically have returned over the
//     elapsed interval; anything beyond that is treated as charging, dropped,
//     and the baseline rebased.
const (
	// maxRegenKw bounds the power regenerative braking can return to the pack.
	// Tesla's own regen ceiling is in the 60-70 kW region across the S/3/X/Y
	// line (higher on Plaid, which this bound is deliberately generous enough
	// to cover). It is an UPPER bound used to reject charging, not an estimate
	// of typical regen: being generous here costs a mis-credited gain on a car
	// that is DC-charging below 70 kW while the drive is still open, and being
	// stingy would silently discard real downhill regen on every mountain
	// drive. The asymmetry favours generosity because rule 1 above catches the
	// charging case outright whenever the car says anything at all.
	maxRegenKw = 70.0

	// maxRegenWindow caps how much elapsed time one gain may be credited
	// against. Energy samples are 30s apart at best and arbitrarily far apart
	// in practice — a connectivity drop, a sleeping car, or a REST-backfill gap
	// can put minutes or hours between two readings — and multiplying
	// maxRegenKw by an unbounded gap would make the rate bound admit any gain
	// at all, which is the same as having no bound. One minute at 70 kW is
	// ~1.17 kWh of admissible regen per step, comfortably more than a real
	// descent returns between two samples and far less than a charging session
	// adds.
	maxRegenWindow = time.Minute
)

// chargingChargeStates are the `chargeState` values that mean the pack is
// taking power from a cable RIGHT NOW. "Complete" and "Stopped" are deliberately
// absent: a car plugged in and finished is not adding energy, so a gain observed
// under those states is regen or noise and the rate bound is the right judge.
// "NoPower" and "Disconnected" are likewise not charging.
var chargingChargeStates = map[string]struct{}{
	"Charging": {},
	"Starting": {},
}

// isChargingState reports whether a `chargeState` reading means energy is
// flowing INTO the pack. An empty string (field absent on this frame) is not
// charging — it is no information, and the caller falls through to the rate
// bound.
func isChargingState(s string) bool {
	_, ok := chargingChargeStates[s]
	return ok
}

// driveEnergy accumulates the energy a drive consumed, in kWh, from the
// `energyRemaining` samples the stream delivers. See the package comment above
// for why it is a running sum rather than a subtraction of two endpoints.
//
// ZERO IS NOT A READING. Every entry point rejects a non-positive
// `energyRemaining`: a car with literally 0 kWh in the pack cannot be driving,
// so a zero is the protobuf/REST zero-value, and admitting one would report an
// entire pack's worth of consumption in a single step. This is the same guard
// the SOC cache applies for the same reason (MYR-207).
type driveEnergy struct {
	// usedKwh is the running sum of credited per-sample consumption. It can go
	// negative on a net-regen drive; total() decides what to do about that.
	usedKwh float64

	// last is the most recent accepted `energyRemaining` reading and lastAt the
	// event time it was taken at. Together they are the baseline the next
	// sample is differenced against.
	last   float64
	lastAt time.Time

	// baselineSet reports whether last/lastAt hold a real observed reading.
	baselineSet bool

	// steps counts CREDITED deltas — samples that produced a consumption
	// figure. A charging rebase does not count, which is what makes
	// "drive spent entirely on a charger" report no energy rather than zero
	// energy.
	steps int

	// chargedInside records that at least one step was rejected as charging.
	// Diagnostic only: it is logged at drive end so an operator can tell a
	// bounded estimate from a clean one, and it does not change the figure.
	chargedInside bool
}

// seed installs the drive's opening baseline from a reading taken BEFORE the
// drive began — the value cached on the vehicle while it sat parked. Energy
// does not move in a parked car that is not plugged in, so the last idle sample
// is the correct drive-start figure, and seeding from it is what lets a drive
// report consumption from its very first in-drive sample instead of from its
// second.
//
// Seeding never credits anything: it establishes the baseline and nothing else.
func (e *driveEnergy) seed(kwh float64, at time.Time) {
	if kwh <= 0 {
		return
	}
	e.last = kwh
	e.lastAt = at
	e.baselineSet = true
}

// observe folds one `energyRemaining` sample into the accumulator.
//
// chargeState is the `chargeState` value carried by the SAME frame, or "" when
// the frame did not carry one. It is consulted only to reject a gain.
func (e *driveEnergy) observe(kwh float64, at time.Time, chargeState string) {
	if kwh <= 0 {
		return
	}
	if !e.baselineSet {
		e.seed(kwh, at)
		return
	}

	// Positive step = energy left the pack = consumption.
	step := e.last - kwh

	switch {
	case step >= 0:
		// Consumption, always credited. A step of exactly zero is a real
		// reading (the car was stationary between samples) and counts as a
		// credited step so a drive that idled still reports 0.0 rather than
		// nothing.
		e.usedKwh += step
		e.steps++
	case isChargingState(chargeState) || -step > e.regenAllowance(at):
		// A gain the car attributes to a cable, or one larger than regen could
		// have produced. Not consumption, not credited, and the baseline
		// rebases to the new level so the energy added does not reappear as
		// consumption on the next step.
		e.chargedInside = true
	default:
		// Regenerative braking. Credited as negative consumption, because the
		// drive's honest figure is the net.
		e.usedKwh += step
		e.steps++
	}

	e.last = kwh
	e.lastAt = at
}

// regenAllowance is the largest gain, in kWh, that regenerative braking could
// have returned since the previous accepted sample. See maxRegenKw and
// maxRegenWindow for why each half of the product is what it is.
//
// A non-positive interval (clock skew between frames, or two samples stamped
// the same instant) yields a zero allowance, so any gain over such an interval
// is treated as charging. That is the fail-closed direction: a mis-classified
// regen loses a fraction of a kWh, a mis-classified charge invents tens.
func (e *driveEnergy) regenAllowance(at time.Time) float64 {
	elapsed := at.Sub(e.lastAt)
	if elapsed <= 0 {
		return 0
	}
	if elapsed > maxRegenWindow {
		elapsed = maxRegenWindow
	}
	return maxRegenKw * elapsed.Hours()
}

// total returns the drive's energy consumption in kWh and whether it is
// reportable at all.
//
// NOT REPORTABLE means the drive never saw two energy readings it could
// difference — a very short drive, a car that streamed nothing but gear, or a
// drive whose every gain was a charging step. The caller writes 0 in that case
// because the column is `NOT NULL`; what makes the zero honest downstream is
// that `Trip.totalEnergyKwh` refuses to sum a window containing one (see
// queryTripDriveTotals).
//
// A NET-REGEN DRIVE REPORTS ZERO, not a negative. Long descents really can end
// with more charge than they started with, and the physical figure is negative,
// but `energyUsedKwh` is summed across a window into one total and a negative
// leg would silently cancel another leg's real consumption — a 200-mile trip
// reporting the efficiency of the 40 miles that happened to be uphill. Zero is
// the clamp, and it is applied here rather than in SQL so every reader of the
// column sees the same rule.
func (e *driveEnergy) total() (float64, bool) {
	if e.steps == 0 {
		return 0, false
	}
	if e.usedKwh < 0 {
		return 0, true
	}
	return e.usedKwh, true
}
