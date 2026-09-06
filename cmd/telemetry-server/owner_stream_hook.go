package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// buildOwnerStreamHook assembles the best-effort post-link stream setup. The
// vehicle lister always targets the Fleet API directly (a read). The fleet
// pusher (a state-changing call that targets a real car) is wired ONLY when the
// tesla-http-proxy + telemetry endpoint are configured — otherwise it stays nil
// and the config push is left to ops/web. This runtime guard is what keeps a
// live push out of any dev/test process. Always returns a non-nil hook.
// reconciler may be nil (the safety guard kept it off); it is assigned through
// an explicit check rather than straight into the interface field, because a
// nil *FleetConfigReconciler in an interface is a NON-nil interface holding a
// nil pointer and would sail past `h.pairing == nil` into a nil-receiver send.
func buildOwnerStreamHook(
	cfg *config.Config,
	upsert vehicleUpserter,
	reconciler *telemetry.FleetConfigReconciler,
	logger *slog.Logger,
) *ownerStreamHook {
	lister := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: cfg.Proxy().FleetAPIBaseURL, // empty => default NA Fleet API
	}, logger.With(slog.String("component", "fleet-list")))

	var pusher fleetConfigPusher
	if cfg.Proxy().URL != "" && cfg.Proxy().FleetTelemetryHostname != "" {
		pusher = &realFleetPusher{
			client: telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
				BaseURL:    cfg.Proxy().URL,
				HTTPClient: proxyHTTPClient(cfg.Proxy().URL, logger),
			}, logger.With(slog.String("component", "fleet-push"))),
			endpoint: telemetry.EndpointConfig{
				Hostname: cfg.Proxy().FleetTelemetryHostname,
				Port:     cfg.Proxy().FleetTelemetryPort,
				CA:       cfg.Proxy().FleetTelemetryCA,
			},
		}
		logger.Info("owner-onboarding fleet-config auto-push enabled")
	} else {
		logger.Warn("owner-onboarding fleet-config auto-push disabled: proxy/telemetry endpoint not configured")
	}

	hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: logger}
	if reconciler != nil {
		hook.pairing = reconciler
	}
	return hook
}

// vehicleLister lists a linked owner's vehicles from the Fleet API.
type vehicleLister interface {
	ListVehicles(ctx context.Context, token string) ([]telemetry.FleetVehicle, error)
}

// vehicleUpserter seeds "Vehicle" identity rows (store.OwnerProvisioner).
//
// MYR-491 widened it with the schedule seed, which is the same actor doing the
// same job one table over: provisioning a car that cannot stream yet, and
// recording WHY. Keeping them on one interface is what guarantees the two
// writes are always wired together — a deployment that provisions vehicles but
// cannot say why they are silent is the state MYR-503 was reported from.
type vehicleUpserter interface {
	UpsertOwnedVehicle(ctx context.Context, in store.OwnedVehicleInput) (store.VehicleUpsertResult, error)
	// SeedFleetConfigSchedule records the link-time schedule row — and, when
	// there is one, the observation behind it (Tesla's `missing_key` skip, or a
	// failed push) — so the owner's first app open can name the one thing left
	// to do instead of waiting up to 45 minutes for the reconciler to discover
	// the same fact, and so the in-band self-heal signals have a row to land on.
	SeedFleetConfigSchedule(ctx context.Context, vin, outcome string, now time.Time) error
}

// NOTE ON WHAT IS DELIBERATELY *NOT* ON THIS INTERFACE (MYR-599). The
// driver-access row — the consent gate — is written by UpsertOwnedVehicle
// itself, from the access type carried on OwnedVehicleInput, inside the same
// transaction as the "Vehicle" row. It was briefly a separate best-effort call
// here, and that was wrong for the one write in this hook whose failure reaches
// a THIRD PARTY: a car provisioned without its gate is indistinguishable from
// an owner's, and the reconciler configures it on the next pass. Keeping it off
// this interface is what stops it drifting back into being skippable.

// fleetConfigPusher pushes the fleet-telemetry config for one VIN so the car
// starts streaming. The real implementation calls the tesla-http-proxy; it is
// injected (and nil unless the proxy is configured) so the SAFETY invariant
// holds: no live push is ever wired in tests or when unconfigured.
type fleetConfigPusher interface {
	PushForVIN(ctx context.Context, token, vin string) error
}

// ownerStreamHook is the best-effort post-link stream setup (MYR-257 steps 2+3):
// list the owner's vehicles, seed their "Vehicle" rows, and (when a real proxy
// is configured) push the fleet-telemetry config per VIN so the car streams
// without an ops `fleet-config push`. Every step is best-effort — a failure is
// logged and never fails the link.
type ownerStreamHook struct {
	lister vehicleLister
	upsert vehicleUpserter
	pusher fleetConfigPusher // nil => push disabled (proxy unconfigured); guard keeps live pushes out of tests
	// pairing receives proof of virtual-key pairing when the link-time push
	// APPLIES (MYR-529). Nil => nobody to tell (no reconciler wired).
	pairing pairingEvidenceNotifier
	logger  *slog.Logger
}

// pairingEvidenceNotifier is the reconciler's inbox for "this VIN's virtual key
// is proven paired". Consumer-site interface, satisfied by
// *telemetry.FleetConfigReconciler.
type pairingEvidenceNotifier interface {
	PairingEvidence(vin string)
}

// AfterLink implements postLinkHook. It is the PASSIVE bulk sync: it provisions
// every owned Fleet-API vehicle and NEVER clears a removed-vehicle tombstone, so
// an incidental re-link can't resurrect a car the owner deliberately removed
// (MYR-261 tombstone-wins). The deliberate re-add path (MYR-262) is the only one
// that clears a tombstone — see ReaddVehicle / VehicleReaddHandler.
func (h *ownerStreamHook) AfterLink(ctx context.Context, userID, accessToken string) {
	vehicles, err := h.lister.ListVehicles(ctx, accessToken)
	if err != nil {
		h.logger.Warn("owner stream setup: list vehicles failed (skipping)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return
	}

	for _, v := range vehicles {
		// MYR-599 REPLACED THE OWNERSHIP FILTER WITH CONSENT. MYR-257 finding 3
		// skipped every non-OWNER vehicle here, silently by design — and the
		// silence was the bug: a tester who linked a car he DRIVES completed
		// OAuth, paired his virtual key, and never saw a row appear or a word
		// about why. Both access types are provisioned now; what separates them
		// is that nothing is pushed at a driver's car until they acknowledge
		// the owner approved it. provisionVehicle owns that branch.
		h.provisionVehicle(ctx, userID, accessToken, v)
	}
}

// ReaddVehicle is the targeted re-provision behind the deliberate re-add
// (MYR-262): after VehicleReaddHandler clears the caller's tombstone, this
// provisions ONLY the single owned car matching teslaVehicleID, reusing the same
// per-vehicle path (provisionVehicle) the passive AfterLink sync uses. It is
// best-effort (returns false, never resurrecting anything, on any list/miss
// condition). Returns whether the car was provisioned.
//
// THE ACCESS CHECK IS NO LONGER A REFUSAL (MYR-599), and the guarantee that
// used to rest on it now rests where it belongs. The old rule — "an OWNER-access
// match is required, so a caller can never attach a car they do not own even
// with a guessed id" — was doing two jobs at once: keeping other people's cars
// off this account, and keeping DRIVER cars off it. The first job is done
// entirely by the FLEET LISTING, which is scoped to this caller's own Tesla
// token: a guessed teslaVehicleID that is not in their fleet still falls through
// to the miss below. The second job is the one the client reversed. So a driver
// may deliberately re-add a car they drive, and it lands in the same
// unacknowledged, un-pushed state a first link would produce.
func (h *ownerStreamHook) ReaddVehicle(ctx context.Context, userID, accessToken, teslaVehicleID string) bool {
	vehicles, err := h.lister.ListVehicles(ctx, accessToken)
	if err != nil {
		h.logger.Warn("owner re-add: list vehicles failed (tombstone cleared; retriable)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return false
	}
	for _, v := range vehicles {
		if v.ID.String() != teslaVehicleID {
			continue
		}
		return h.provisionVehicle(ctx, userID, accessToken, v)
	}
	h.logger.Warn("owner re-add: target not in caller fleet (tombstone cleared, nothing to provision)",
		slog.String("user_id", userID),
		slog.String("tesla_vehicle_id", teslaVehicleID))
	return false
}

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
func (h *ownerStreamHook) provisionVehicle(ctx context.Context, userID, accessToken string, v telemetry.FleetVehicle) bool {
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

// signalPairing hands proven pairing to the reconciler so a car that is not yet
// streaming gets its hot schedule from the very first door (MYR-529).
//
// Best-effort and non-blocking, like every other step in this hook: the
// notifier is a buffered inbox drained by the reconcile loop, and a deployment
// with no reconciler (no signing proxy) simply has nobody to tell.
func (h *ownerStreamHook) signalPairing(userID, vin string) {
	if h.pairing == nil {
		return
	}
	h.logger.Info("owner stream setup: link-time config push APPLIED — virtual key already paired, reconciler notified",
		slog.String("event", "fleet_config_pairing_at_link"),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)))
	h.pairing.PairingEvidence(vin)
}
