package main

// The DRIVER-ACCESS half of ownerStreamHook (MYR-599): what happens after a car
// the linking account DRIVES but does not own has been provisioned.
//
// Split from owner_stream_hook.go for the CLAUDE.md 300-line file cap; the hook
// and this file are one component.
//
// ── WHAT CHANGED, AND WHY THE OLD BEHAVIOUR WAS NOT SAFE, ONLY SILENT ────────
//
// MYR-257 finding 3 put an ownership filter at the top of the provisioning
// loop: every Fleet-API vehicle whose `access_type` was not OWNER produced an
// `owner_vehicle_skipped reason=not_owner` line and nothing else. The intent was
// sound — never attach somebody else's car to this account — but the mechanism
// conflated two different protections:
//
//   - "don't attach a car this person has no relationship with". That is done by
//     the FLEET LISTING, which is scoped to the caller's own Tesla token, and by
//     UpsertOwnedVehicle's cross-user rule. Neither depends on access_type.
//   - "don't act on a car this person does not own". That is a real concern, and
//     it is the one this file now answers — but the answer is CONSENT, not
//     absence.
//
// On 2026-09-05 a tester linked a car he drives on somebody else's Tesla
// account. OAuth completed, the token was stored, he paired the virtual key —
// and no "Vehicle" row was ever created, so the app had nothing to show and
// nothing to explain. The filter was silent by design; the silence was the bug.
//
// ── THE SHAPE OF THE REPLACEMENT ─────────────────────────────────────────────
//
// The car IS provisioned. Nothing is pushed at it. The gap between those two
// sentences is the whole feature: a driver-linked car exists, can be named, can
// be seen, and keeps the virtual key its linker may already have paired — but no
// fleet-telemetry config reaches it, from ANY path, until §7.29 records that
// its owner approved adding it.
//
// WHAT THIS RECORDS IS EVIDENCE, NOT PERMISSION. The platform cannot verify an
// owner's approval with Tesla — no API exposes it — so what it holds instead is
// an attributable record that a named account, at a named instant, was shown a
// named version of the text and agreed. That is the honest artifact, and it is
// deliberately the part that outlives the account (the AuditLog row), while the
// standing gate does not.

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// provisionDriverAccess completes the SHUT-GATE exit of provisionVehicle: the
// "Vehicle" row already exists, so this records WHY it is inert and leaves it
// that way.
//
// IT RUNS ONLY WHILE THE GATE IS SHUT (MYR-599 review finding D), which is a
// narrower condition than "Tesla calls this person a driver" and the difference
// is not cosmetic. `awaiting_owner_ack` means "nothing has ever been pushed at
// this car". It is a member of store.fleetConfigAbsentOutcomes, so the MYR-592
// inactivity sweeper skips any car carrying it — correctly, since there is no
// Tesla-side config to remove and no disconnection to warn anybody about.
//
// The old fork seeded it on EVERY driver-branch pass. A driver car that had been
// acknowledged months ago and had been streaming ever since would, on the next
// incidental AfterLink, have the label written back over whatever the reconciler
// had recorded — asserting "never configured" about a car that was configured
// and billing, and re-arming the sweeper exemption for good. The cost control
// would have been permanently disabled for exactly the population MYR-599
// created. An ACKNOWLEDGED driver car now takes the ordinary push-and-seed path,
// which is the same one an owner's car takes and the correct one for a car whose
// gate is open.
//
// EVERYTHING IT DOES IS BEST-EFFORT, AND THAT IS ONLY SAFE BECAUSE THE GATE IS
// NOT DONE HERE. The two writes carry very different weights:
//
//   - The DRIVER-ACCESS ROW is the gate. Every push path in the server refuses a
//     car that has one with a NULL acknowledged_at, so a car MISSING it is
//     indistinguishable from an owner's and the reconciler configures it on its
//     next pass — unattended, at a car whose owner never agreed to anything. Its
//     failure is the only one in this hook that reaches a third party, which is
//     exactly why it is NOT a step here: UpsertOwnedVehicle writes it in the
//     same transaction as the "Vehicle" row, so the car and its gate exist
//     together or not at all.
//   - The SCHEDULE SEED, below, is explanation. If it is missing the card is
//     quieter than it should be — and, since the MYR-599 review, the inactivity
//     sweeper still refuses the car, because it anti-joins the driver-access row
//     itself rather than trusting this label.
//
// It is also why the setup-state derivation reads the DRIVER ROW rather than the
// schedule label: the authoritative fact must not be the one carried by the
// write that is allowed to be absent.
func (h *ownerStreamHook) provisionDriverAccess(ctx context.Context, userID, vin string) {
	// THE GATE IS ALREADY WRITTEN by the time this runs. UpsertOwnedVehicle
	// recorded the driver-access row in the SAME transaction as the "Vehicle"
	// row, from the access signal carried on OwnedVehicleInput — Tesla's
	// access_type mapped through the one exported spelling of the fail-closed
	// rule, with the RAW token stored verbatim beside it so the column can still
	// answer the only question it exists for: what did Tesla actually say?
	//
	// That atomicity is the point. This function is best-effort — everything it
	// does now is explanation — precisely BECAUSE the one thing that must not be
	// best-effort already happened, indivisibly, upstream.
	//
	// Seeded INSTEAD OF a push, which makes this the one schedule label in the
	// system that describes a push that never happened.
	h.seedSetupSchedule(ctx, userID, vin, store.SetupOutcomeAwaitingOwnerAck)
}

// logDriverAccess emits the line that replaced `owner_vehicle_skipped
// reason=not_owner`.
//
// INFO, redacted VIN, and deliberately a DIFFERENT event name rather than a new
// reason on the old one: this is not a skip. A car was provisioned, and any
// operator or query reading the old event as "nothing happened" would now be
// wrong.
//
// It fires for BOTH gate states, and `acknowledged` is what separates them. An
// acknowledged driver car goes on to be pushed at exactly like an owner's — but
// it is still not owned by the person who linked it, and emitting
// `owner_vehicle_owned` for it would make the one audit trail that can answer
// "how did this car get here?" say something false.
func (h *ownerStreamHook) logDriverAccess(userID, vin, accessType string, pending bool) {
	h.logger.Info("owner_vehicle_driver_access",
		slog.String("event", "owner_vehicle_driver_access"),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)),
		slog.String("access_type", accessType),
		slog.Bool("acknowledged", !pending))
}
