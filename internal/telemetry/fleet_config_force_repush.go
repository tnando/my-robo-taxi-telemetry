// The MYR-489 escalation: synced-but-not-streaming on a car we can prove is
// awake earns ONE forced config re-push per pairing epoch.
//
// MYR-448 taught the reconciler to notice this state and say so
// (`fleet_config_synced_not_streaming`), then back off — on the theory that a
// car reporting a synced config is merely asleep. Tester Nabil's car disproved
// the theory in prod: signed commands applying (so awake, so paired), Tesla
// reporting the config synced, and zero receiver connections for two days. The
// config record existed at Tesla and the car had simply never acted on it.
//
// Contrast James Guan's car in the same state, which started streaming minutes
// after pairing with no intervention. Both must be handled by the same loop
// with no operator, which is what makes this an ESCALATION rather than a policy
// change: the ordinary backoff still runs, and the forced push fires only when
// the cheap explanations are exhausted and the expensive one is evidenced.

package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// Outcome labels for the escalation path. Same vocabulary rules as the MYR-448
// labels: internal, never wire-exposed, never a raw Tesla body.
const (
	outcomeForcedRepush     = "forced_repush"
	outcomeForcedRepushFail = "forced_repush_failed"
)

// fleetVehicleStateReader reads Tesla's own connectivity state for a VIN. Like
// GetTelemetryConfig this is an UNSIGNED authenticated read and MUST target the
// direct Fleet API, never the signing proxy.
type fleetVehicleStateReader interface {
	GetVehicle(ctx context.Context, token, vin string) (*FleetVehicleState, error)
}

// fleetTelemetryConfigDeleter removes a VIN's config. It carries no body to
// sign, so it rides the proxy's plain-forward path exactly like the per-VIN
// GET (see DeleteTelemetryConfig's contract note) and is safe on the writer
// client.
type fleetTelemetryConfigDeleter interface {
	DeleteTelemetryConfig(ctx context.Context, token, vin string) error
}

// handleSyncedQuiet decides what to do about a candidate whose config Tesla
// reports as synced while the car stays silent.
//
// Returns after either escalating or rescheduling; the caller is done with the
// vehicle either way.
func (r *FleetConfigReconciler) handleSyncedQuiet(
	callCtx, schedCtx context.Context,
	c FleetConfigCandidate,
	accessToken, vin string,
	out *ReconcileOutcome,
) {
	out.AlreadySynced++

	// The MYR-448 line, unchanged: it is what tells an operator to stop looking
	// at fleet-config, and it is now also the marker of the FIRST observation
	// that arms the escalation on the next pass.
	r.logger.Info("fleet-config reconcile: config already synced but vehicle is quiet",
		slog.String("event", "fleet_config_synced_not_streaming"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.String("last_outcome", c.LastOutcome),
		slog.Time("last_updated", c.LastUpdated))

	reason, ok := r.escalationBlocker(c)
	if !ok {
		r.logger.Info("fleet-config reconcile: not escalating a synced-but-quiet vehicle",
			slog.String("event", "fleet_config_escalation_declined"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("reason", reason))
		r.reschedule(schedCtx, c, outcomeSyncedQuiet)
		return
	}

	if !r.provenAwake(callCtx, c, accessToken, vin) {
		r.reschedule(schedCtx, c, outcomeSyncedQuiet)
		return
	}

	r.forceRepush(callCtx, schedCtx, c, accessToken, vin, out)
}

// forceRepush performs the escalation: DELETE the existing config, then create
// it again.
//
// WHY DELETE-THEN-CREATE AND NOT A PLAIN OVERWRITE. The overwrite is what the
// ordinary path already does, with the identical body, and Tesla's answer to it
// is the `synced: true` that got us here — the config record at Tesla is
// already correct, so re-POSTing it changes no state the car would notice. What
// is stuck is the car's own telemetry session, and the only lever the Fleet API
// gives us over that is the existence of the config record itself. Deleting it
// and creating it again produces a genuinely new record for the firmware to
// re-read.
//
// THE WINDOW. Between the DELETE and the POST the car has no config. That is
// acceptable precisely because this path only runs against a car that is not
// streaming — there is no working session to interrupt — and the window is one
// HTTP round-trip. If the POST nevertheless fails, the car is left worse than
// we found it, so that case gets its own loud event and a retry at the BASE
// interval rather than a doubled backoff.
//
// CONNECTING MID-FLIGHT — an accepted, bounded risk. Candidates are listed at
// the start of a pass, so a car could in principle open its stream between the
// listing and its turn, and be re-configured while it is coming up. The window
// is one pass at most, the car must ALSO have been silent across two separate
// observations to reach this branch, and the worst outcome is that the same
// config it just read is re-issued and it re-syncs. Closing the window would
// mean a fresh per-vehicle staleness read against the Prisma "Vehicle" table
// immediately before every force, which buys a few seconds of precision on an
// event that is already several orders of magnitude rarer than the two-day
// silence this exists to end.
func (r *FleetConfigReconciler) forceRepush(
	callCtx, schedCtx context.Context,
	c FleetConfigCandidate,
	accessToken, vin string,
	out *ReconcileOutcome,
) {
	r.logger.Info("fleet-config reconcile: forcing a config re-push on an awake but silent vehicle",
		slog.String("event", "fleet_config_forced_repush"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Time("signed_command_at", c.SignedCommandAt))

	// A delete failure is NOT fatal to the escalation — a 404 for a config that
	// is already gone reads the same as a transient error here, and the POST
	// below is the part that matters. It is logged and the push proceeds.
	deleted := true
	if err := r.deps.Writer.DeleteTelemetryConfig(callCtx, accessToken, c.VIN); err != nil {
		deleted = false
		r.logger.Warn("fleet-config reconcile: forced re-push could not delete the existing config",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
	}

	result, err := r.deps.Writer.PushTelemetryConfig(callCtx, accessToken, r.request(c.VIN))
	if err != nil || SkipErrorFor(result, c.VIN) != nil {
		// MYR-599 review finding G: THIS DOOR HAD TO LEARN THE SAME LESSON THE
		// ORDINARY PUSH LEARNED, and it is the door that mattered most.
		//
		// `push` already classified Tesla's owner-only refusal out of the
		// transient bucket; this one did not, so it recorded
		// `forced_repush_failed` — which the derivation reads as `configuring`
		// for a whole 24-hour window. That is precisely the slow lie MYR-491's
		// honesty bar exists against, and it was reachable on the ONE route a
		// driver-access car actually takes: the §7.29 acknowledge endpoint's own
		// best-effort push runs through ForceConfigRepushNow, not through
		// `push`. So the first thing a driver saw after consenting was a card
		// saying "connecting…" about a car Tesla had just refused outright.
		if isOwnerAccessRefusal(err) {
			out.PushFailures++
			out.OwnerAccessRefusals++
			r.recordOwnerAccessRefusedForce(schedCtx, c, vin, deleted, err)
			return
		}
		r.recordFailedForce(schedCtx, c, vin, deleted, err)
		out.PushFailures++
		return
	}

	out.ForcedRepushes++
	r.logger.Info("fleet-config reconcile: forced re-push applied; vehicle should re-establish its stream",
		slog.String("event", "fleet_config_forced_repush_applied"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Int("updated_vehicles", result.Response.UpdatedVehicles))

	// Deliberately NOT ClearFleetConfigAttempts. An ordinary repair clears the
	// schedule because it fixed a config that was genuinely missing; this one
	// pushed a config that already existed, on a hypothesis about the car's
	// firmware. Recording it as a normal incrementing attempt — while stamping
	// the epoch budget as spent — is what guarantees that a car which STILL
	// does not stream falls back into plain backoff instead of re-forcing every
	// pass.
	r.recordForce(schedCtx, c, r.now().Add(r.nextAttemptGap(c, outcomeForcedRepush)), outcomeForcedRepush)
}

// recordFailedForce handles a forced re-push whose create step did not apply.
func (r *FleetConfigReconciler) recordFailedForce(
	ctx context.Context, c FleetConfigCandidate, vin string, deleted bool, err error,
) {
	msg := "fleet-config reconcile: forced re-push failed"
	level := slog.LevelWarn
	if deleted {
		// The delete succeeded and the create did not: the car now has NO
		// config at all. Recoverable — the retry below is at the base interval,
		// and the ordinary unconfigured path will push a fresh one — but it is
		// the one state this feature can leave behind that is worse than doing
		// nothing, so it is an error-level, uniquely greppable event.
		msg = "fleet-config reconcile: forced re-push deleted the config but could not recreate it — vehicle is UNCONFIGURED"
		level = slog.LevelError
	}
	r.logger.Log(ctx, level, msg,
		slog.String("event", "fleet_config_forced_repush_failed"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Bool("config_deleted", deleted),
		slog.String("error", redactedErrorText(err)))

	// Base interval, not the doubled backoff: whatever else is true, this car
	// may now be unconfigured, and the ordinary push path can fix that. Inside
	// a hot pairing epoch the ladder's gap is used instead, which is shorter
	// still — the one state worse than "silent" deserves the fastest repair the
	// schedule can offer, not a fifteen-minute one. The epoch budget is stamped
	// either way, so this cannot become a forced-push loop.
	retry := r.cfg.Interval
	if hot := r.nextAttemptGap(c, outcomeForcedRepushFail); hot < retry {
		retry = hot
	}
	r.recordForce(ctx, c, r.now().Add(retry), outcomeForcedRepushFail)
}

// recordForce writes the attempt and stamps the pairing epoch's escalation
// budget as spent. A bookkeeping failure is logged, never fatal — but it is
// worse here than elsewhere, because an unstamped budget is a budget that can
// be spent again, so it says so.
func (r *FleetConfigReconciler) recordForce(
	ctx context.Context, c FleetConfigCandidate, next time.Time, outcome string,
) {
	if err := r.deps.Attempts.RecordForcedFleetConfigRepush(ctx, c.VehicleID, r.now(), next, outcome); err != nil {
		r.logger.Warn("fleet-config reconcile: could not record forced re-push (epoch budget not stamped)",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("outcome", outcome),
			slog.String("error", err.Error()))
	}
}

// recordOwnerAccessRefusedForce handles a forced re-push Tesla refused outright
// (MYR-599).
//
// IT IS A DIFFERENT EVENT FROM recordFailedForce EVEN THOUGH THE CAR IS IN THE
// SAME PHYSICAL STATE, and the difference is what a retry is worth. A failed
// force may have left the car unconfigured and the ordinary push path can repair
// that within one interval. This one may ALSO have left it unconfigured — the
// delete is permitted for a driver token even though the create is not — and
// nothing we do will repair it: Tesla will refuse the create every time until
// either DRIVER-token config POST ships or the car's real owner adds it under
// their own account.
//
// So the schedule records the standing refusal rather than a transient failure,
// which is what lets §7.0 / §7.1 say `owner_access_required` instead of decaying
// into `configuring`, and what puts the vehicle on the long fixed back-off
// rather than a doubling ladder against an answer that cannot change.
//
// The delete-succeeded case still earns ERROR: whatever the cause, a car we left
// with no config is the one state this feature can produce that is worse than
// doing nothing, and an operator should be able to grep for it.
func (r *FleetConfigReconciler) recordOwnerAccessRefusedForce(
	ctx context.Context, c FleetConfigCandidate, vin string, deleted bool, err error,
) {
	msg := "fleet-config reconcile: forced re-push refused — Tesla will not configure this VIN for this authorization (retrying will not help)"
	level := slog.LevelWarn
	if deleted {
		msg = "fleet-config reconcile: forced re-push DELETED the config and Tesla refused to recreate it — vehicle is UNCONFIGURED and cannot be reconfigured by this account"
		level = slog.LevelError
	}
	r.logger.Log(ctx, level, msg,
		slog.String("event", "fleet_config_owner_access_required"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Bool("config_deleted", deleted),
		slog.String("error", redactedErrorText(err)))

	// recordForce rather than reschedule, so the epoch's escalation budget is
	// still stamped as spent: an unstamped budget is a budget that can be spent
	// again, and re-forcing against a standing refusal would delete a config
	// once per epoch for nothing.
	r.recordForce(ctx, c, r.now().Add(ownerAccessRetryGap), outcomeOwnerAccessRequired)
}
