package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// THE PER-VEHICLE BODY of the post-link stream setup — split out of
// owner_stream_hook.go, which MYR-601 pushed further past the 300-line rule it
// was already over.
//
// The seam is the one the two callers already draw: owner_stream_hook.go owns
// the two DOORS (the passive AfterLink bulk sync and the deliberate MYR-262
// re-add) and what they share — the lister, the seams, the once-per-pass
// widening. This file owns what happens to ONE car once the row is written,
// which is where every branch in this hook actually lives.

// provisionVehicle seeds one vehicle's "Vehicle" row and then takes ONE OF TWO
// EXITS, decided by WHETHER THE CONSENT GATE ON THAT ROW IS SHUT.
//
//   - GATE SHUT (an unacknowledged driver-access row): the schedule is seeded
//     `awaiting_owner_ack` and NOTHING IS PUSHED. The car is provisioned — it
//     appears, it can be named, the virtual key the person may already have
//     paired is not wasted — but it is inert until §7.29 records that the owner
//     approved adding it.
//   - GATE OPEN (an owner's car, OR a driver's car whose acknowledgment is
//     already on record): best-effort push of the fleet-telemetry config,
//     exactly as before.
//
// THE FORK USED TO BE "IS THIS PERSON THE OWNER?" AND THAT WAS THE BUG (MYR-599
// review finding D). Every AfterLink pass over an ALREADY-ACKNOWLEDGED,
// happily streaming driver-access car took the driver branch and re-seeded
// `awaiting_owner_ack` — a label whose entire purpose is to say "nothing was
// ever pushed at this car". It is in `fleetConfigAbsentOutcomes`, so the MYR-592
// inactivity sweeper exempts a car carrying it; re-seeded on every link, the
// exemption became permanent, and an acknowledged driver car would stream and
// bill forever with the cost control switched off for it. The gate's STATE is
// the honest fork, and the acknowledged case is one the owner branch already
// handles correctly.
//
// It is the shared per-vehicle body of both the passive AfterLink sync and the
// deliberate ReaddVehicle path — the tombstone gate lives inside
// UpsertOwnedVehicle, so a still-tombstoned car is skipped here (returns false)
// regardless of caller and regardless of access type. Returns whether the car
// was provisioned.
func (h *ownerStreamHook) provisionVehicle(
	ctx context.Context, userID, accessToken string, v telemetry.FleetVehicle, gain *accessGain,
) bool {
	vin := v.VIN
	res, err := h.upsert.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{
		UserID:         userID,
		TeslaVehicleID: v.ID.String(),
		VIN:            vin,
		Name:           v.DisplayName,
		// MYR-599: the access type travels WITH the provisioning write, so the
		// consent gate is created in the same transaction as the car it gates.
		// It used to be a second, best-effort round trip — which meant a failed
		// gate write left the car provisioned and indistinguishable from an
		// owner's, for the reconciler to configure unattended.
		//
		// TeslaAccessType is the RAW token, stored verbatim to answer "what did
		// Tesla say?"; Access is the tri-state INTERPRETATION, built by the one
		// exported spelling of the fail-closed rule so this file and the store
		// cannot disagree about what counts as ownership.
		TeslaAccessType: v.AccessType,
		Access:          store.AccessSignalFor(v.AccessType),
	})
	if err != nil {
		h.logger.Warn("owner stream setup: vehicle upsert failed (skipping vehicle)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return false
	}
	if res.Outcome == store.VehicleSkippedTombstoned {
		// The owner deliberately removed this VIN (MYR-261 tombstone). A passive
		// re-link must NOT resurrect it — skip and do NOT push config for a car
		// the owner offboarded. Cleared only by a deliberate re-add (MYR-262).
		h.logger.Info("owner_vehicle_skipped",
			slog.String("event", "owner_vehicle_skipped"),
			slog.String("user_id", userID),
			slog.String("reason", "removed_tombstone"),
			slog.String("vin", redactVIN(vin)))
		return false
	}
	if res.Outcome == store.VehicleSkippedCrossUser {
		// The teslaVehicleId belongs to another user under a claim this link
		// does not outrank — either an owner's row (the driver-wins-nothing
		// direction of MYR-599's owner-wins rule) or an owner-versus-owner
		// collision. Never reassigned. Audit and do NOT push config.
		h.logger.Warn("owner_vehicle_skipped",
			slog.String("event", "owner_vehicle_skipped"),
			slog.String("user_id", userID),
			slog.String("reason", "cross_user_teslaVehicleId"),
			slog.String("vin", redactVIN(vin)))
		return false
	}
	if res.Outcome == store.VehicleOwnedByTransfer {
		// MYR-599 OWNER WINS: this car was provisioned by somebody who only
		// drives it, and its real owner has just linked. The row moved to them
		// in the provisioning transaction, along with the teardown of the
		// previous linker's gate, schedule and shares. INFO, because nothing
		// failed and this is the designed resolution — but loudly enough to be
		// greppable, because a car changed hands and the former driver was not
		// told.
		h.logger.Info("owner_vehicle_transferred_from_driver",
			slog.String("event", "vehicle_driver_link_superseded_by_owner"),
			slog.String("user_id", userID),
			slog.String("vehicle_id", res.VehicleID),
			slog.String("vin", redactVIN(vin)))
	}
	if res.AccessDowngradeObserved {
		// MYR-599: Tesla answered something other than OWNER for a car we have
		// held all along under owner access, and the provisioning transaction
		// REFUSED to gate it. Never silent: this is either a Fleet listing that
		// omitted access_type (the benign and, so far, only observed cause) or a
		// genuine access downgrade — which MYR-599 explicitly does not handle,
		// and which would need its own issue if this line ever became common.
		h.logger.Warn("owner_vehicle_access_downgrade_observed",
			slog.String("event", "owner_vehicle_access_downgrade_observed"),
			slog.String("user_id", userID),
			slog.String("vehicle_id", res.VehicleID),
			slog.String("vin", redactVIN(vin)),
			slog.String("access_type", v.AccessType))
	}

	// MYR-601: THE CAR IS ON THE ROW, SO THE ACCESS SET HAS ALREADY CHANGED.
	// Recorded here rather than at either exit below, because both of them
	// provisioned a car: an unacknowledged driver-access vehicle is in its
	// linker's access set exactly like an owner's — the consent gate holds the
	// Tesla-side PUSH, not the row, and §7.0 lists the car either way. Placed
	// before the fork so no future branch can return past it.
	//
	// The GAIN lands in the caller's accumulator and is announced once the pass
	// is over; the transfer's LOSSES are published from inside this call,
	// immediately. See announceProvisioned for why the two differ.
	h.announceProvisioned(res, gain)

	// MYR-599: the fork. Taken BEFORE the `owner_vehicle_owned` line below,
	// because that line's whole meaning is "this account owns this car" and
	// emitting it for a driver's car would make the one audit trail that can
	// answer "how did this car get here?" say the wrong thing. The DRIVER half
	// covers the acknowledged case too, so the log stays honest about what kind
	// of car this is even when the push path below is the one that runs.
	if res.DriverAccessPresent {
		h.logDriverAccess(userID, vin, v.AccessType, res.DriverAccessPending)
	} else {
		h.logger.Info("owner_vehicle_owned",
			slog.String("event", "owner_vehicle_owned"),
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)))
	}

	if res.DriverAccessPending {
		h.provisionDriverAccess(ctx, userID, vin)
		return true
	}
	// NOTE the access-UPGRADE case is already handled: UpsertOwnedVehicle
	// deleted any stale driver row in the same transaction that reconciled this
	// car, because the signal was OWNER. That ordering is not a convention to
	// remember here — it is a property of the one statement above.

	// MYR-517: the seed is UNCONDITIONAL and idempotent, and it is the last
	// thing this function does on every path that provisioned a car. See
	// seedSetupSchedule for why the push outcome may vary but the write may not.
	pushOutcome, applied := h.pushConfig(ctx, userID, accessToken, vin)
	h.seedSetupSchedule(ctx, userID, vin, pushOutcome)
	if applied {
		// MYR-529: the link-time push landed, which Tesla only does for an
		// enrolled virtual key. Signalled AFTER the seed, so the reconciler's
		// pairing reset lands on a row that exists rather than creating a
		// second one, and the epoch is stamped on the same row the card reads.
		h.signalPairing(userID, vin)
	}
	return true
}
