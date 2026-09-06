// MYR-599's driver-access fact, on the telemetry side of the store boundary:
// the raw row, the ONE question every push path asks of it, and the two-member
// wire enum derived from it.
//
// WHY THE TYPE IS DECLARED TWICE (here and as store.VehicleDriverAccess). The
// same reason VehicleSetupSchedule is: internal/telemetry must not import
// internal/store, so the boundary is crossed by an explicit field-by-field copy
// in cmd/telemetry-server (driverAccessRow), and a field added on one side and
// forgotten on the other fails to compile there rather than silently reading as
// its zero value here.
//
// THE ZERO VALUE POINTS TWO DIFFERENT WAYS, and this is the one thing to hold
// in mind when touching anything downstream:
//
//   - For the WIRE it is SAFE. Present false means owner access, which is what
//     every car in the fleet was before MYR-599 and what an absent
//     `teslaAccessType` means to a consumer on an older server.
//   - For the GATE it is UNSAFE. Present false means "nothing to acknowledge,
//     push away". So a hand-built row, or a row from a read path that does not
//     join go_vehicle_driver_access, must never be gated on. Every gate in this
//     package sits behind a GetByID call that does join it — that is not a
//     coincidence to preserve by convention, it is why the field lives on
//     VehicleSnapshotRow rather than being fetched separately.

package telemetry

import "time"

// Wire values for VehicleSummary.teslaAccessType / VehicleState.teslaAccessType
// (rest-api.md §7.0, §7.1, contracts v0.39.0).
//
// TWO MEMBERS AND NO MORE, deliberately, even though Tesla's own access_type is
// an open string we store verbatim. The client has exactly two things to render
// — "your car" or "you drive this car" — and every non-OWNER answer Tesla has
// ever given, including an EMPTY one, means the second. Widening this enum to
// mirror Tesla's vocabulary would export an upstream detail no consumer can act
// on and would make an unknown future value a client-side branch instead of a
// server-side fail-closed.
const (
	// TeslaAccessTypeOwner — Tesla reports the linking account as the OWNER of
	// this vehicle. The DEFAULT reading of an absent field, which is what makes
	// the addition backward-compatible.
	TeslaAccessTypeOwner = "owner"
	// TeslaAccessTypeDriver — Tesla reports the linking account as holding
	// DRIVER access: they drive the car, somebody else owns it.
	TeslaAccessTypeDriver = "driver"
)

// VehicleDriverAccess is one vehicle's go_vehicle_driver_access row, or the
// absence of one, as LEFT JOINed by the snapshot and catalog reads. RAW
// STORAGE — both wire values are derived from it here and neither is emitted
// from it.
type VehicleDriverAccess struct {
	// Present is true when Tesla reported the linking account as something
	// other than the OWNER of this car.
	Present bool
	// CreatedAt is when the driver-access row was recorded — the `since` the
	// `awaiting_owner_acknowledgment` setup state carries. Zero when absent.
	CreatedAt time.Time
	// AcknowledgedAt is when the driver acknowledged that the owner approved
	// adding this car. Zero means NOT YET, which is the shut gate.
	AcknowledgedAt time.Time
}

// PendingAcknowledgment reports whether this car is still waiting on its
// driver's acknowledgment — the single question every config-push path asks,
// and the condition behind the `awaiting_owner_acknowledgment` setup state.
//
// A method rather than an inlined `Present && AcknowledgedAt.IsZero()` because
// there are six call sites in this package alone, and a gate spelled six ways
// is a gate that will eventually be spelled five ways.
func (d VehicleDriverAccess) PendingAcknowledgment() bool {
	return d.Present && d.AcknowledgedAt.IsZero()
}

// teslaAccessTypeWire returns the §7.0 / §7.1 value for this row.
//
// PRESENCE DECIDES, not the raw access_type string: the row exists only because
// a Fleet API listing said something other than OWNER (including saying
// nothing), so its existence IS the claim. The raw token stays in the database
// for the operator question — what did Tesla actually say — and reaches only the
// ops listings, never a consumer.
//
// ALWAYS EMITTED by this server version, never omitted: a consumer reads an
// ABSENT field as `"owner"`, so an omitted "owner" would be correct but would
// make a pre-v0.39.0 server and a driver-linked car indistinguishable to
// anything inspecting the wire.
func teslaAccessTypeWire(d VehicleDriverAccess) string {
	if d.Present {
		return TeslaAccessTypeDriver
	}
	return TeslaAccessTypeOwner
}
