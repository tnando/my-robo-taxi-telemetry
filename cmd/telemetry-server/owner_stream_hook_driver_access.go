package main

// The DRIVER-ACCESS half of ownerStreamHook (MYR-599): provisioning a car the
// linking account drives but does not own, and un-provisioning that fact when
// Tesla later calls the same account the owner.
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
// fleet-telemetry config reaches it, from ANY path, until §7.24 records that
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
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// provisionDriverAccess completes the DRIVER exit of provisionVehicle: the
// "Vehicle" row already exists, so this records WHAT KIND of car it is and
// leaves it inert.
//
// THE ORDER OF THE TWO WRITES IS NORMATIVE AND IT IS NOT THE OBVIOUS ONE.
// The driver-access row goes first, the schedule seed second, because the two
// carry different weights:
//
//   - The DRIVER-ACCESS ROW is the gate. Every push path in the server refuses a
//     car that has one with a NULL acknowledged_at. If it is missing, the car is
//     indistinguishable from an owner's and the reconciler will push at it on
//     its next pass — silently, unattended, at a car whose owner never agreed to
//     anything. This write failing is the one failure in this hook with a
//     consequence outside our own user.
//   - The SCHEDULE SEED is explanation. If it is missing the card is quieter
//     than it should be, and the reconciler fills it in later.
//
// So the row is attempted first and its failure is logged at ERROR while the
// seed's is a WARN, and NEITHER fails the link — which is the hook's standing
// contract and is also why the setup-state derivation reads the DRIVER ROW
// rather than the schedule label: the authoritative fact must not be the one
// carried by the best-effort write that is allowed to be absent.
func (h *ownerStreamHook) provisionDriverAccess(ctx context.Context, userID string, v telemetry.FleetVehicle) {
	vin := v.VIN

	// Tesla's access_type VERBATIM, including the EMPTY string older Fleet
	// responses have shipped. FleetVehicle.IsOwner already treats empty as
	// non-owner — fail closed, an unknown access level must never be promoted to
	// ownership — and storing '' rather than inventing "DRIVER" keeps the column
	// able to answer the only question it exists for: what did Tesla actually
	// say?
	if err := h.upsert.RecordDriverAccess(ctx, vin, userID, v.AccessType, time.Now()); err != nil {
		// ERROR, not WARN, and the log line says what is now true rather than
		// what failed: without the row this car is provisioned WITH THE CONSENT
		// GATE OPEN, and the next reconciler pass will configure it. That is the
		// one outcome in this hook that reaches a third party.
		h.logger.Error("owner stream setup: driver-access row could not be written — this car is provisioned with NO acknowledgment gate and the reconciler may configure it",
			slog.String("event", "owner_vehicle_driver_access_write_failed"),
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)),
			slog.String("access_type", v.AccessType),
			slog.String("error", err.Error()))
	}

	// Seeded INSTEAD OF a push, which makes this the one schedule label in the
	// system that describes a push that never happened. It is what lets the
	// schedule row say why it is sitting there, and what keeps the MYR-592
	// inactivity sweeper from "disconnecting" a car that was never connected.
	h.seedSetupSchedule(ctx, userID, vin, store.SetupOutcomeAwaitingOwnerAck)

	// Replaces `owner_vehicle_skipped reason=not_owner`. INFO, redacted VIN, and
	// deliberately a DIFFERENT event name rather than a new reason on the old
	// one: this is not a skip. A car was provisioned, and any operator or query
	// reading the old event as "nothing happened" would now be wrong.
	h.logger.Info("owner_vehicle_driver_access",
		slog.String("event", "owner_vehicle_driver_access"),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)),
		slog.String("access_type", v.AccessType))
}

// clearDriverAccess drops a stale driver-access row when Tesla now reports this
// account as the car's OWNER.
//
// THE ACCESS-UPGRADE CASE, and it is a real one: a title transfer, or an owner
// who had been reaching their own car through a second account. The row is
// evidence about a claim that is no longer true, and leaving it would keep the
// wire saying `teslaAccessType: "driver"` about a car this person owns outright
// — and, if it was never acknowledged, would hold the push gate shut on a car
// that needs nobody's permission.
//
// IT DOES NOT RUN THE OTHER WAY, and the gap is deliberate rather than
// overlooked: Tesla DOWNGRADING an owner to a driver is not observed here,
// because nothing re-lists an already-provisioned owner's cars for that purpose.
// A car that changes hands at Tesla keeps streaming to its old linker until they
// re-link or remove it. Recorded in the PR; out of scope for MYR-599.
//
// Best-effort, like every step in this hook: a failure is logged and the link
// still succeeds. The cost of a miss is the stale-row state described above,
// which the next OWNER re-link clears.
func (h *ownerStreamHook) clearDriverAccess(ctx context.Context, userID, vin string) {
	if err := h.upsert.ClearDriverAccess(ctx, vin, userID); err != nil {
		h.logger.Warn("owner stream setup: stale driver-access row could not be cleared (car may read as driver-access until the next owner re-link)",
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()))
	}
}
