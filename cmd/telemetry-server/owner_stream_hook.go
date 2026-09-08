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
// access carries the MYR-601 access-set seam (bust + widen + the transfer's
// narrow). Every field of it may be nil — see ownerStreamAccess.
func buildOwnerStreamHook(
	cfg *config.Config,
	upsert vehicleUpserter,
	reconciler *telemetry.FleetConfigReconciler,
	access ownerStreamAccess,
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

	hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, access: access, logger: logger}
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
	// access is the MYR-601 access-set seam: provisioning a car is an
	// access-set WIDENING, and until this existed the link path was the one
	// widening producer that announced nothing. See
	// owner_stream_hook_access.go. Zero value = announce nothing.
	access ownerStreamAccess
	logger *slog.Logger
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

	// MYR-601 review round: ONE widening for the whole pass. A first link of a
	// three-car fleet is one access-set change, and announcing per car closed
	// every session this owner held three times — each close provoking a
	// reconnect that raced the next — to deliver a fact the first re-handshake
	// already delivered in full. See accessGain.
	var gain accessGain
	for _, v := range vehicles {
		// MYR-599 REPLACED THE OWNERSHIP FILTER WITH CONSENT. MYR-257 finding 3
		// skipped every non-OWNER vehicle here, silently by design — and the
		// silence was the bug: a tester who linked a car he DRIVES completed
		// OAuth, paired his virtual key, and never saw a row appear or a word
		// about why. Both access types are provisioned now; what separates them
		// is that nothing is pushed at a driver's car until they acknowledge
		// the owner approved it. provisionVehicle owns that branch.
		h.provisionVehicle(ctx, userID, accessToken, v, &gain)
	}
	h.flushGain(userID, gain)
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
		// ONE car, so the accumulator flushes immediately and the deliberate
		// re-add keeps the single announce it always had (MYR-601).
		var gain accessGain
		provisioned := h.provisionVehicle(ctx, userID, accessToken, v, &gain)
		h.flushGain(userID, gain)
		return provisioned
	}
	h.logger.Warn("owner re-add: target not in caller fleet (tombstone cleared, nothing to provision)",
		slog.String("user_id", userID),
		slog.String("tesla_vehicle_id", teslaVehicleID))
	return false
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
