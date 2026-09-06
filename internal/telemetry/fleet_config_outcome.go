// The per-pass tally for the MYR-448 fleet-config reconciler, and the one log
// line that reports it.
//
// Split from fleet_config_reconciler.go for the CLAUDE.md 300-line file cap;
// the pass logic and this file are one component.

package telemetry

import "log/slog"

// ReconcileOutcome counts what one pass did, for the completion log line and
// for tests to assert against.
type ReconcileOutcome struct {
	Examined      int
	AlreadySynced int
	Repaired      int
	AwaitingKey   int
	SkippedOther  int
	TokenFailures int
	ReadFailures  int
	PushFailures  int
	// ForcedRepushes counts MYR-489 escalations that applied: a
	// synced-but-quiet car, proven awake, whose config was deleted and
	// recreated to make its firmware re-establish the telemetry session.
	ForcedRepushes int
	// OwnerAccessRefusals counts pushes Tesla refused with its owner-only
	// `404 … not_found` for a VIN the same token can still LIST (MYR-599).
	//
	// COUNTED SEPARATELY FROM PushFailures, which it also increments, because
	// the two need opposite operator responses: a push failure is a thing to
	// retry, and this is a thing that will never succeed until either Tesla
	// ships DRIVER-token config creation or the car's real owner adds it under
	// their own account. It is also how ForceConfigRepushNow reports the
	// distinction back to §7.23 / §7.29, which answer `owner_access_required`
	// rather than a 502.
	OwnerAccessRefusals int
	// AlreadyStreaming counts candidates that were found to be streaming from
	// the listing row alone (MYR-529) — examined, resolved, and costing nothing
	// upstream. Distinct from AlreadySynced, which means Tesla holds a config
	// for a car that is NOT streaming.
	AlreadyStreaming int
	// Truncated is set when the pass ended early (shutdown or an expired
	// budget) rather than after examining every candidate — without it a pass
	// that did almost nothing logs identically to a complete one.
	Truncated bool
}

func (r *FleetConfigReconciler) logPassComplete(out ReconcileOutcome) {
	r.logger.Info("fleet-config reconcile: pass complete",
		slog.Int("examined", out.Examined),
		slog.Int("already_streaming", out.AlreadyStreaming),
		slog.Int("already_synced", out.AlreadySynced),
		slog.Int("repaired", out.Repaired),
		slog.Int("awaiting_virtual_key", out.AwaitingKey),
		slog.Int("skipped_other", out.SkippedOther),
		slog.Int("token_failures", out.TokenFailures),
		slog.Int("read_failures", out.ReadFailures),
		slog.Int("push_failures", out.PushFailures),
		slog.Int("forced_repushes", out.ForcedRepushes),
		slog.Int("owner_access_refusals", out.OwnerAccessRefusals),
		slog.Bool("truncated", out.Truncated))
}
