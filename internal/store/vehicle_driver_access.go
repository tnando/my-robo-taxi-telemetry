// The READ half of MYR-599: how a car's DRIVER-ACCESS row reaches the four
// vehicle-read surfaces that need it.
//
// ONE EXPRESSION AND ONE JOIN, composed by constant into the snapshot read and
// all three §7.0 catalog producers (owner, shared/viewer, group-ride member),
// for exactly the reason catalogTelemetrySuspendedExpr and setupScheduleColumns
// are written this way: four hand-copied fragments is four chances for one of
// them to drift, and the member query — which reuses the SHARED scan — fails
// LOUDLY on a scan-order mismatch rather than silently. That loudness is a
// property of composing the SELECT lists from the same constants; keep it.
//
// TWO THINGS ARE DERIVED FROM THE SAME TWO COLUMNS, and they are different
// facts that must not be collapsed:
//
//   - ROW PRESENCE answers "does Tesla call this person a driver rather than
//     the owner of this car?" — the §7.0 / §7.1 wire field `teslaAccessType`,
//     which reads an absent row as `"owner"`.
//   - `acknowledged_at IS NULL` answers "may the platform push a
//     fleet-telemetry config at this car yet?" — the gate every push path
//     consults, and the `awaiting_owner_acknowledgment` setup state.
//
// A car can be a driver car AND fully acknowledged, which is the ordinary
// steady state after the sheet: it streams like any other car and still says
// `teslaAccessType: "driver"` so the client can render "you drive this car".
//
// WHY PRESENCE IS READ OFF created_at AND NOT A SYNTHETIC BOOLEAN — the exact
// opposite of what setupScheduleColumns does, so it is worth saying why the two
// differ rather than leaving a reader to assume one of them is wrong.
// go_fleet_config_attempts has NO non-nullable payload column, so presence
// there had to be projected as `fca.vehicle_id IS NOT NULL`. Here `created_at`
// is NOT NULL in the table (migration 0046), so the ONLY way it can arrive NULL
// is the LEFT JOIN finding no row — which makes it a sound presence signal and
// saves the catalog a third column on a path AGENTS.md holds to a lean
// projection.
//
// A READ of a Go-owned table joined into a read of the Prisma-owned "Vehicle".
// CG-DL-9 governs MIGRATIONS, not runtime SELECTs.

package store

import (
	"context"
	"fmt"
	"time"
)

// VehicleDriverAccess is one vehicle's go_vehicle_driver_access row, or the
// absence of one. RAW STORAGE: both wire values derived from it — the
// `teslaAccessType` enum and the `awaiting_owner_acknowledgment` setup state —
// are produced in internal/telemetry and never emitted from here.
type VehicleDriverAccess struct {
	// Present is true when this car was linked by someone Tesla reports as a
	// DRIVER rather than the OWNER. False is the overwhelmingly common case and
	// the safe default on any hand-built row: it reads as owner access, which is
	// what every car in the fleet was before MYR-599.
	Present bool
	// CreatedAt is when the driver-access row was recorded — the instant the
	// `awaiting_owner_acknowledgment` state carries as its `since`. Zero when
	// Present is false.
	CreatedAt time.Time
	// AcknowledgedAt is when the driver acknowledged that the owner approved
	// adding this car. ZERO MEANS NOT YET, and that is the gate: no config is
	// pushed at a car whose driver row is unacknowledged.
	AcknowledgedAt time.Time
}

// PendingAcknowledgment reports whether this car is waiting on its driver to
// acknowledge the owner's approval — the single question every push path asks.
//
// Written as a method rather than inlined at each call site because there are
// seven of them (the reconciler's SQL, complete-setup, reconnect, the two
// fleet-config push routes, the acknowledge endpoint's own re-check, and the
// setup-state derivation), and a gate spelled seven ways is a gate that will
// eventually be spelled six ways.
func (d VehicleDriverAccess) PendingAcknowledgment() bool {
	return d.Present && d.AcknowledgedAt.IsZero()
}

// catalogDriverAccessExpr projects the driver-access row onto a vehicle read.
//
// TWO fixed-width timestamps off a primary-key probe, and no more: the raw
// `tesla_access_type` token is deliberately NOT selected here. The wire enum has
// two members and row presence decides between them, so pulling Tesla's raw
// string would be a column the response body does not emit — the exact thing
// the AGENTS.md lean-projection invariant forbids. The raw token stays in the
// table for the operator question ("what did Tesla actually say?") and reaches
// only the ops listings, which are allowed to be wide.
const catalogDriverAccessExpr = `dva.created_at, dva.acknowledged_at`

// driverAccessJoin attaches the driver-access row. Written against the quoted
// "Vehicle" relation rather than an alias so it composes with the unaliased
// snapshot/owner queries and the aliased shared/member ones, exactly as
// setupScheduleJoin and catalogTelemetrySuspendedJoin do.
const driverAccessJoin = `
LEFT JOIN go_vehicle_driver_access dva ON dva.vehicle_id = "Vehicle"."id"`

// driverAccessScan is the destination bag for catalogDriverAccessExpr, in the
// same shape and for the same reason as setupScheduleScan: the column order
// (which tracks the SQL) and the field mapping (which tracks the domain type)
// live in separate methods so they fail independently and loudly rather than
// silently transposing two same-typed timestamps.
type driverAccessScan struct {
	createdAt      *time.Time
	acknowledgedAt *time.Time
}

// dests returns the scan destinations in catalogDriverAccessExpr's order.
func (d *driverAccessScan) dests() []any {
	return []any{&d.createdAt, &d.acknowledgedAt}
}

// value collapses the scanned row into the domain type. A NULL created_at is
// the LEFT JOIN reporting no row at all (the column is NOT NULL in the table),
// which is owner access; a NULL acknowledged_at on a present row is the gate
// still shut.
func (d *driverAccessScan) value() VehicleDriverAccess {
	if d.createdAt == nil {
		// Defensive rather than merely tidy, matching setupScheduleScan: an
		// absent row must carry no residue, so a future caller cannot read a
		// stray timestamp off it.
		return VehicleDriverAccess{}
	}
	return VehicleDriverAccess{
		Present:        true,
		CreatedAt:      *d.createdAt,
		AcknowledgedAt: derefTime(d.acknowledgedAt),
	}
}

// queryPendingDriverAcknowledgmentByVIN answers, for one VIN, the question every
// config-push path asks: is this car still waiting on its driver's
// acknowledgment?
//
// KEYED BY VIN because its one caller is the VIN-keyed fleet-config push route,
// which never resolves a cuid. An EXISTS rather than a row read: the caller
// wants the gate, not the row, and the partial index
// idx_go_vehicle_driver_access_pending covers exactly this predicate.
//
// A car with no "Vehicle" row, or with no driver-access row, yields false —
// owner access, the ordinary case. That is the RIGHT default here and the only
// place in this file where a false is not merely safe but correct: the caller
// has already established that this VIN belongs to this user.
const queryPendingDriverAcknowledgmentByVIN = `
SELECT EXISTS (
    SELECT 1
    FROM go_vehicle_driver_access dva
    JOIN "Vehicle" v ON v."id" = dva.vehicle_id
    WHERE v."vin" = $1
      AND dva.acknowledged_at IS NULL
)`

// PendingDriverAcknowledgmentByVIN reports whether the car with this VIN is a
// driver-linked car whose owner-approval acknowledgment has not been recorded
// (MYR-599).
//
// THE CALLER MUST FAIL CLOSED ON THE ERROR. This is a consent gate protecting
// somebody who is not our user, so "we could not tell" must never be spent as
// "go ahead" — unlike almost every other best-effort read in this package, whose
// worst case is a quieter card. The signature returns the error rather than
// folding it into the bool precisely so a caller cannot ignore it by accident.
func (r *VehicleRepo) PendingDriverAcknowledgmentByVIN(ctx context.Context, vin string) (bool, error) {
	var pending bool
	if err := r.pool.QueryRow(ctx, queryPendingDriverAcknowledgmentByVIN, vin).Scan(&pending); err != nil {
		return false, fmt.Errorf("VehicleRepo.PendingDriverAcknowledgmentByVIN(%s): %w", redactVIN(vin), err)
	}
	return pending, nil
}

// unacknowledgedDriverAccessGate is the SQL half of the same question
// PendingAcknowledgment answers in Go — "is this car waiting on its driver's
// acknowledgment?" — for the one consumer that cannot ask in Go: the
// reconciler's candidate listing, which must exclude such cars from a set it
// builds in a single statement.
//
// Written against the `v` alias the candidate query uses. The partial index
// idx_go_vehicle_driver_access_pending (migration 0046) is built over exactly
// this predicate, so the anti-join is an index probe rather than a scan.
//
// A NOT EXISTS rather than a LEFT JOIN + IS NULL, matching the tombstone guard
// directly above it in that query: the semi-join cannot duplicate a candidate
// row even if the table's primary key were ever relaxed, and it reads as what
// it is — a gate, not a projection.
const unacknowledgedDriverAccessGate = `
        SELECT 1
        FROM go_vehicle_driver_access dva
        WHERE dva.vehicle_id = v."id"
          AND dva.acknowledged_at IS NULL`

// pendingOwnerAckExprV / pendingOwnerAckExprVeh project the SAME question as a
// BOOLEAN COLUMN on a candidate row, rather than using it to filter one out.
//
// WHY BOTH A FILTER AND A COLUMN EXIST, which looks redundant and is not. The
// reconciler has TWO candidate producers: the periodic pass
// (queryFleetConfigCandidates, which filters with the NOT EXISTS above) and the
// pairing-signal path (queryResetFleetConfigScheduleOnPairing, which resets one
// named car's schedule and hands the row straight to reconcileOne). Gating only
// the first left a live hole — a driver who linked a borrowed car and sent ONE
// signed command would drive the reconciler into a config read, a push, and
// potentially the MYR-489 forced re-push's DELETE, all against a third party's
// car and none of it consented to.
//
// So every producer now REPORTS the fact and the single consumer enforces it.
// The filter stays on the periodic pass because excluding a row is cheaper than
// carrying it, but the column is what makes the invariant hold: a third producer
// added later inherits the gate instead of silently reopening it.
//
// IN THE PERIODIC QUERY THE COLUMN IS ALWAYS FALSE, and that is not an
// oversight to tidy away. The same statement's NOT EXISTS has already removed
// every row that could make it true, so the projection is, today, a constant —
// and hardcoding the constant is precisely what would make the gate fragile.
// The rule this package is trying to hold is that EVERY producer of a
// FleetConfigCandidate reports the field truthfully, so the one consumer can
// enforce it without knowing which door a row came through. A `false` literal
// would encode "this producer's rows are never pending" as a fact about the
// code rather than a fact about the data, and the day somebody relaxes or
// reorders that filter — to admit a car for a cheap streaming check, say — the
// column would keep confidently answering false and the hole would be open
// again with no test failing. The redundancy is the invariant paying for
// itself: it costs an index probe on rows the planner has already narrowed, and
// it buys a guarantee that survives an edit to a WHERE clause three files away.
//
// Two spellings only because the two statements name the vehicle relation
// differently — `v` for the candidate listing, the `veh` CTE for the pairing
// reset. The predicate is otherwise identical, and identical to the gate above.
const pendingOwnerAckExprV = `EXISTS (
        SELECT 1 FROM go_vehicle_driver_access dva
        WHERE dva.vehicle_id = v."id" AND dva.acknowledged_at IS NULL
      )`

const pendingOwnerAckExprVeh = `EXISTS (
        SELECT 1 FROM go_vehicle_driver_access dva
        WHERE dva.vehicle_id = veh."id" AND dva.acknowledged_at IS NULL
      )`
