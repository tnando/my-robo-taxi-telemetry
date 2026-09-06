// The CONSENT GATE half of the provisioning transaction (MYR-599): deciding
// whether a car gets a driver-access row, and reporting the state of the one it
// ends up with.
//
// Split from owner_vehicle_provision.go for the CLAUDE.md 300-line file cap; the
// upsert and these statements are one component and run in one transaction.

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyDriverAccess settles this car's driver-access row inside the provisioning
// transaction (MYR-599). THREE directions now, and the third is the fix.
//
//   - OWNER → delete any row that is there. The access-UPGRADE case: a title
//     transfer, or an owner who had been reaching their own car through a second
//     account. A stale row would keep the wire saying `teslaAccessType:
//     "driver"` about a car this person owns outright and, if never
//     acknowledged, would hold the push gate shut on a car needing nobody's
//     permission.
//
//   - UNKNOWN → touch NOTHING, and report the row's existing state. The caller
//     made no claim, so this transaction has no basis for changing a consent
//     fact. It still READS the row, because "we did not look" must not be
//     reported to the caller as "there is no gate" — that reversal is exactly
//     the fail-open the tri-state was introduced to make unspellable.
//
//   - NOT OWNER → write the gate, but ONLY on an INSERT or onto a car that
//     ALREADY carries a driver-access row.
//
// THAT LAST BOUND IS THE WHOLE POINT OF THIS FUNCTION'S REWRITE. The rule used
// to be an unconditional upsert, which meant one Fleet listing that omitted
// `access_type` — a shape older Fleet API responses have genuinely shipped, and
// which the fail-closed reading maps to DRIVER — would file a driver-access row
// against a car its real owner had been streaming for months. Every push path
// then refuses that car, the reconciler stops repairing it, the inactivity
// sweeper stops counting it, and the owner's app shows a sheet asking them to
// confirm that somebody else approved adding their own vehicle. There is no way
// out of that state except the acknowledgment, which is a lie for them to sign.
//
// A NEW car is different and is still gated: nothing is established, nobody is
// streaming, and an unknown access level on a first sighting is exactly the case
// fail-closed exists for. So the closure is kept where it is safe and dropped
// where it is destructive. TRUE ACCESS DOWNGRADE HANDLING — Tesla demoting an
// owner to a driver on a car we already hold — is explicitly out of scope for
// MYR-599; what this records instead is that the signal was SEEN and refused, so
// the caller can say so and a future issue has a log line to start from.
//
// The upsert deliberately does NOT touch acknowledged_at or created_at: a
// re-link must refresh what Tesla currently says without re-shutting a gate the
// person already opened, or every incidental re-link would demand a second
// acknowledgment for a car that is already streaming. Consent, once given, is
// not withdrawn by a background sync.
func applyDriverAccess(ctx context.Context, tx pgx.Tx, res *VehicleUpsertResult, in OwnedVehicleInput) error {
	switch in.Access {
	case AccessSignalOwner:
		if _, err := tx.Exec(ctx, queryDeleteDriverAccessByVehicle, res.VehicleID); err != nil {
			return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): clear driver access: %w", in.UserID, err)
		}
		return nil

	case AccessSignalUnknown:
		pending, present, err := readDriverAccessState(ctx, tx, res.VehicleID)
		if err != nil {
			return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): read driver access: %w", in.UserID, err)
		}
		res.DriverAccessPresent, res.DriverAccessPending = present, pending
		return nil

	default: // AccessSignalDriver
		var pending bool
		err := tx.QueryRow(ctx, queryGateDriverAccessByVehicle,
			res.VehicleID, in.UserID, in.TeslaAccessType, res.Inserted).Scan(&pending)
		if errors.Is(err, pgx.ErrNoRows) {
			// The statement's WHERE refused it: an ESTABLISHED row with no
			// driver-access row of its own. Not an error — a refusal, and the
			// caller logs it.
			res.AccessDowngradeObserved = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): record driver access: %w", in.UserID, err)
		}
		res.DriverAccessPresent = true
		res.DriverAccessPending = pending
		return nil
	}
}

// queryGateDriverAccessByVehicle writes the consent gate, BOUNDED.
//
// The `WHERE $4 OR EXISTS (…)` is applyDriverAccess's third rule in SQL: $4 is
// "this statement inserted the vehicle row", and the EXISTS is "this car already
// carries a gate". Neither true means an established owner's car received a
// non-OWNER signal, the INSERT selects no source row, nothing is written, and
// the RETURNING yields pgx.ErrNoRows — which the caller reads as the refusal it
// is rather than as a failure.
//
// Doing it in ONE statement rather than a probe-then-write keeps the decision
// and the write in the same snapshot: a concurrent acknowledgment cannot land
// between them and be clobbered by a gate written on stale evidence.
//
// RETURNING (acknowledged_at IS NULL) hands back the gate's state for both arms
// — a fresh insert is always unacknowledged, a conflict update reports whatever
// the standing row says — so the caller learns whether the gate is shut without
// a second round trip.
//
// No `"userId"` guard: the id came from an upsert whose own WHERE clause already
// refused to touch a car belonging to anybody else, so there is no window in
// which the owner could have changed.
const queryGateDriverAccessByVehicle = `
INSERT INTO go_vehicle_driver_access (vehicle_id, user_id, tesla_access_type)
SELECT $1, $2, $3
WHERE $4 OR EXISTS (SELECT 1 FROM go_vehicle_driver_access WHERE vehicle_id = $1)
ON CONFLICT (vehicle_id) DO UPDATE
SET tesla_access_type = EXCLUDED.tesla_access_type
RETURNING (acknowledged_at IS NULL)`

// readDriverAccessState answers "is there a gate on this car, and is it shut?"
// without writing anything — the UNKNOWN-signal arm, and the transfer probe.
func readDriverAccessState(ctx context.Context, tx pgx.Tx, vehicleID string) (pending, present bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT (acknowledged_at IS NULL) FROM go_vehicle_driver_access WHERE vehicle_id = $1`,
		vehicleID).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read driver-access state(%s): %w", vehicleID, err)
	}
	return pending, true, nil
}
