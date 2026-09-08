-- MYR-620 (migration 0053): one banner per (trip, destination) per half hour.
--
-- ── WHAT HAPPENED ───────────────────────────────────────────────────────────
--
-- 2026-09-08, a client screenshot: TEN "Tesla is on the move — Heading to
-- Element by Marriott Sedona." banners on one lock screen in 59 minutes, five
-- of them inside a single minute. Every one of them was a correctly-claimed,
-- once-per-leg `trip_leg_started` push. The leg is what flapped (MYR-612:
-- a transient destination-name delta closed the leg and the next frame reopened
-- it), and `go_trip_legs.started_notified_at` cannot see that — it is a claim on
-- a ROW, and each reopen was a new row.
--
-- MYR-612's debounce and resume make the flap far rarer. This table makes the
-- BANNER bounded whatever the detector does, which is the property the person
-- holding the phone actually cares about: the same trip going to the same place
-- does not announce itself twice in half an hour, however many legs the server
-- opened underneath.
--
-- ── WHY A SEPARATE CLAIM AND NOT A WIDER ONE ────────────────────────────────
--
-- The four per-leg stamps stay exactly what they are: they arbitrate DELIVERY
-- of one leg's four independent sends, and each must remain retryable on its
-- own. This is a different question — "has this person already been told this
-- sentence recently" — asked of the trip rather than of the leg, and answering
-- it on a leg row is impossible by construction.
--
-- ── THE KEY IS A DIGEST, NOT A NAME ─────────────────────────────────────────
--
-- `destination_key` is a SHA-256 of the normalised destination name, computed
-- by internal/trips, and the plaintext never reaches this table. A destination
-- is P1 (data-classification.md §1.18) — a place a car actually drove to — and
-- every other column that holds one in this schema is sealed. Equality is the
-- only operation this predicate needs, so a digest is not a compromise: it is
-- the whole requirement, at P0.
--
-- `event` is in the key so a departure and an arrival suppress independently.
-- They are different sentences about the same journey and the second is the one
-- that reports the outcome.
--
-- CG-DL-9: go_trip_legs / go_trips are Go-owned; no Prisma-owned relation is
-- named here.

CREATE TABLE IF NOT EXISTS go_trip_leg_banners (
    trip_id         TEXT        NOT NULL REFERENCES go_trips (id) ON DELETE CASCADE,

    -- The push event this slot arbitrates: `trip_leg_started` or
    -- `trip_leg_arrived`. A string rather than an enum for the same reason the
    -- rest of this schema avoids them — a sixth event must not need a
    -- migration.
    event           TEXT        NOT NULL,

    -- P0. SHA-256 (hex) of the normalised destination name. See the header.
    destination_key TEXT        NOT NULL,

    -- When the banner this slot represents was last actually sent.
    last_sent_at    TIMESTAMPTZ NOT NULL,

    -- The CLAIM's conflict target. One row per (trip, event, destination),
    -- updated in place, so the table is bounded by a trip's distinct
    -- destinations rather than by its legs.
    PRIMARY KEY (trip_id, event, destination_key)
);

COMMENT ON TABLE go_trip_leg_banners IS
    'MYR-620. One row per (trip, leg event, destination digest): when that '
    'banner was last sent. The claim refuses a re-send inside '
    'Config.LegBannerWindow, so a flapping leg detector cannot spam a lock '
    'screen. Rows die with their trip.';
