// Package fleetrepush is the MYR-630 fleet-config RE-PUSH sweep: sending the
// CURRENT DefaultFieldConfig to every car that already streams.
//
// ── THE PROBLEM IT EXISTS FOR ───────────────────────────────────────────────
//
// A fleet-telemetry config reaches a car exactly once, when it is pushed. Tesla
// stores no version and no hash, and nothing in this system re-pushes a healthy
// car: the MYR-448 reconciler's candidate query selects cars that have gone
// QUIET, which is the precise complement of the set that matters here. So every
// change to DefaultFieldConfig — a new field, a new interval, MYR-629's
// `ResendIntervalSeconds` on EnergyRemaining — is DORMANT for the whole existing
// fleet until an operator re-pushes it, and dormant silently: the cars keep
// streaming, they simply stream the old field set.
//
// This sweep is that operator action. It is not a background pass, and
// deliberately so — it costs one Tesla write per car in the fleet and changes
// what every car emits, which is a decision with a bill attached and therefore
// one a human takes, having first read the dry run.
//
// ── DRY RUN IS THE DEFAULT ──────────────────────────────────────────────────
//
// With Apply false nothing is pushed. Every car is still listed, its config is
// still READ from Tesla (an unsigned GET), and the report says what would
// happen to it and why. Two writes can still happen on a dry run and both are
// deliberate:
//
//   - an OAuth token refresh, when a stored token has expired. Tesla's refresh
//     tokens are single-use, so not persisting the new pair would break the
//     owner's next server-side call. Serialized through the account row lock
//     (MYR-595), never the unguarded path.
//   - the MYR-447 operator-decrypt audit row, one per owner whose token is read.
//     A failure to write it aborts the run; an unattributable decrypt is the
//     thing the audit exists to prevent.
//
// ── HOW A CONFIG'S AGE IS KNOWN ─────────────────────────────────────────────
//
// There is no "pushed at" anywhere. Every push in this codebase sets `exp` to
// exactly 350 days from the moment it was sent, so Tesla's echoed `exp` dates
// the push: age = 350d - (exp - now). Tesla documents `synced` but not that it
// echoes `exp`, so a nil exp reads as "unknown age" rather than as zero — and an
// unknown age never changes what the sweep does, it only leaves a column blank.
//
// That arithmetic is also the sweep's own verification: re-run the dry run after
// an --apply and every pushed car reports an age of roughly zero.
//
// ── IDEMPOTENCE ─────────────────────────────────────────────────────────────
//
// A push REPLACES a car's config, so running the sweep twice leaves the fleet in
// the same state as running it once; the second run simply re-sends an identical
// body. The sweep writes nothing of its own to our database — no attempt rows,
// no schedule — so nothing accumulates across runs and no run has to be
// completed or rolled back. A capped run is not resumable in the sense of
// picking up where it stopped; it is re-runnable, which is stronger.
package fleetrepush
