-- MYR-612: a per-(device, leg) stamp on the push-to-start registry.
--
-- ── WHAT HAPPENED ───────────────────────────────────────────────────────────
--
-- A leg's Live Activity is PUSH-TO-START: the server creates the card, because
-- a leg begins while nobody's phone is doing anything. The fan-out runs ONCE,
-- at the instant the leg opens, over whatever tokens are registered then.
--
-- On 2026-09-08 the only participant's phone registered its token at 03:40:27
-- — THREE SECONDS after the leg it was for opened at 03:40:24, because the
-- registration is what his phone did on receiving the `trip_leg_started` push.
-- The fan-out had already run and found no tokens; nothing sends a
-- push-to-start when a token arrives DURING an open leg. `go_live_activities`
-- held zero leg rows: no card for anybody, ever, on that leg.
--
-- The fix is a CATCH-UP: registering a token while the trip has an open leg
-- starts that leg's card on that device immediately. This column is what makes
-- the catch-up and the leg-open fan-out unable to double-send.
--
-- ── WHY A COLUMN AND NOT A GUESS ────────────────────────────────────────────
--
-- The leg already carries `activity_started_at`, but that is a LEG-level claim
-- — "the fan-out for this leg has run" — and it cannot answer the per-device
-- question the catch-up asks, which is "has THIS phone been sent this leg's
-- card". Two Live Activities for one journey on one lock screen is the failure
-- this stamp exists to make structurally impossible, and ActivityKit ROTATES
-- push-to-start tokens, so "the app re-registered" is an ordinary event rather
-- than a duplicate.
--
-- BOTH SENDERS CLAIM THROUGH IT, before sending: the leg-open fan-out and the
-- catch-up. A claim is `SET started_leg_id = $leg WHERE started_leg_id IS
-- DISTINCT FROM $leg RETURNING …`, so whichever gets there first sends and the
-- other finds nothing to do. The registration upsert RESETS it to NULL when the
-- token VALUE changes, which is exactly right: a rotated token addresses a
-- phone that holds no card for this leg, and it must be able to get one.
--
-- CG-DL-9: go_trip_activity_tokens is Go-owned; no Prisma-owned relation is
-- named here.
--
-- P0: an opaque leg cuid on a row that already names the trip and the user. It
-- discloses nothing the row did not already carry, and it is NOT the token.

ALTER TABLE go_trip_activity_tokens
    ADD COLUMN IF NOT EXISTS started_leg_id TEXT;

COMMENT ON COLUMN go_trip_activity_tokens.started_leg_id IS
    'MYR-612. The leg whose push-to-start was last sent to this device, claimed '
    'before the send by both the leg-open fan-out and the registration catch-up '
    'so the two cannot raise two cards for one journey. NULL means no card has '
    'been started for this registration; the upsert resets it when the token '
    'value rotates, because a new token addresses a phone holding no card.';
