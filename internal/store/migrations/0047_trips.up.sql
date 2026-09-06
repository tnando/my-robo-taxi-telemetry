-- 0047_trips.up.sql
--
-- MYR-602: TRIPS — an owner-defined time WINDOW on one vehicle during which a
-- chosen subset of the vehicle's accepted share-holders is a `trip_participant`
-- rather than a plain `viewer`.
--
-- ── WHAT A TRIP IS, IN ONE SENTENCE ─────────────────────────────────────────
--
-- A trip is a (vehicle, window, participant set) tuple. It creates NO new
-- vehicle relationship: every participant already holds an accepted
-- go_vehicle_shares grant on the car (that is where the picker's candidates
-- come from), and the trip decides only what that grant means BETWEEN two
-- instants. Access therefore cannot outlive the share, and the participant
-- query joins the live grant rather than trusting these rows — see
-- internal/auth/queries.go queryActiveTripVehicleIDs.
--
-- ── FOUR TABLES, AND WHY THE LEG ONE EXISTS ─────────────────────────────────
--
--   go_trips                 the window itself, plus the sweeper's stamps
--   go_trip_participants     who is in it, and when they left
--   go_trip_activity_tokens  ActivityKit PUSH-TO-START tokens, per (trip, user)
--   go_trip_legs             one row per driving leg the car takes in a window
--
-- The leg table is the one that could plausibly have been left out — a leg is
-- derived from telemetry and is over in an hour — and it is here for two
-- reasons that are both about EXACTLY-ONCE:
--
--   1. The five `trips` pushes include `trip_leg_started` and
--      `trip_leg_arrived`, which must fire ONCE per leg per participant. The
--      only way to make a push idempotent across a restart, a redeploy or two
--      arrival signals in the same second is a durable stamp, and the stamp
--      needs a row to live on.
--   2. The per-leg Live Activity needs a durable ANCHOR. go_live_activities
--      rows are keyed to the thing the Activity is ABOUT so the updater can
--      find them again; for a ride that is the ride, and for a trip it has to
--      be the leg (a trip may run for days and contain a dozen legs, each with
--      its own Activity). Migration 0047 therefore also widens
--      go_live_activities with a `trip_leg_id` anchor beside `ride_request_id`
--      and a CHECK that EXACTLY ONE of the two is set.
--
-- ── CG-DL-9: NO PRISMA TABLES ───────────────────────────────────────────────
--
-- `vehicle_id`, `owner_user_id` and `user_id` are plain TEXT columns holding
-- Prisma cuids, with NO foreign key, for the same reason migrations 0031, 0044
-- and 0046 hold none: a file under internal/store/migrations/ may not NAME a
-- Prisma-owned table, and the CI gate greps for the identifier. The rows are
-- reached explicitly by the account-deletion sequence (step 8g) and by the
-- vehicle teardown instead. Foreign keys BETWEEN the four Go-owned tables here
-- are permitted and are used, because a dangling participant row on a deleted
-- trip would be a genuine ambiguity in an access gate.
--
-- ── CLASSIFICATION ──────────────────────────────────────────────────────────
--
-- P1: `go_trips.name_enc` and `go_trip_legs.destination_name_enc` are USER
-- CONTENT and a PLACE NAME respectively, both sealed with AES-256-GCM through
-- the same label encryptor that seals Vehicle."destinationName" (MYR-447,
-- internal/store/label_encryption.go). A trip name is chosen by a person and
-- routinely names where they are going ("DFW → LA"); a leg destination is a
-- place a car actually drove to, which data-classification.md §1.18 already
-- classifies P1 wherever it is stored.
--
-- P1 (capability): `go_trip_activity_tokens.push_to_start_token` is an APNs
-- push-to-start token — the same tier as go_live_activities.activity_push_token
-- and handled identically. NOT encrypted at rest for the reason §3.2 gives for
-- its sibling (the sender needs the exact bytes on every push, and a round trip
-- buys nothing against an attacker who also holds the signing key). Log
-- redaction is the control: only push.tokenPrefix's 8 characters are ever
-- logged, and the value is never echoed into a response or an error envelope.
--
-- P0: everything else here is opaque cuids, instants and booleans.

-- ─────────────────────────────────────────────────────────────────────────────
-- go_trips
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS go_trips (
    -- Caller-minted cuid, same shape as go_ride_requests.id.
    id            TEXT        PRIMARY KEY,

    -- The car. Opaque Prisma cuid, no FK (CG-DL-9).
    vehicle_id    TEXT        NOT NULL,

    -- The owner AT CREATION TIME, denormalised so the owner-only mutations can
    -- be gated without a join to a table this file may not name. It is NOT the
    -- authority: every owner-gated endpoint re-resolves ownership through
    -- ResolveVehicleAccess, because a car can change hands and this column
    -- would then be stale in the permissive direction.
    owner_user_id TEXT        NOT NULL,

    -- The trip's name, AES-256-GCM sealed (P1 user content). NOT NULL: the
    -- create endpoint requires 1..60 characters after trimming, so there is no
    -- nameless trip and therefore no absent sentinel to express.
    name_enc      TEXT        NOT NULL,

    -- The window. `starts_at` MAY be in the past at creation time — that is how
    -- the legs of a road trip already driven join the trip retroactively, and
    -- it is a stated product requirement, not an accident to guard against.
    starts_at     TIMESTAMPTZ NOT NULL,
    ends_at       TIMESTAMPTZ NOT NULL,

    -- Set when the OWNER ENDS THE TRIP EARLY. The effective end of a window is
    -- LEAST(ends_at, ended_at), computed in every reader rather than written
    -- back over ends_at: overwriting would destroy the owner's stated intent
    -- and make an accidental early end unexplainable.
    ended_at      TIMESTAMPTZ,

    -- The sweeper's idempotency stamps. Each of the two lifecycle pushes fires
    -- at most once per trip, and "at most once" survives a restart only if the
    -- fact is durable. Nullable instants rather than booleans for the same
    -- reason migration 0044 chose instants: the timestamp answers the operator
    -- question the boolean cannot.
    started_notified_at TIMESTAMPTZ,
    ended_notified_at   TIMESTAMPTZ,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- A window with no duration is not a window. Enforced HERE rather than only
    -- in the handler because these two columns are also written by PATCH, and a
    -- validation that lives in one of two writers is a validation that holds
    -- half the time.
    CONSTRAINT go_trips_window_ordered CHECK (ends_at > starts_at),

    -- The 30-day cap. A ceiling on how long a standing live-location grant can
    -- be created in one gesture, so an owner cannot mistype a year and hand out
    -- a decade of access. INTERVAL arithmetic on TIMESTAMPTZ is exact for a
    -- fixed number of days, so this is a real bound and not an approximation.
    CONSTRAINT go_trips_window_capped
        CHECK (ends_at <= starts_at + INTERVAL '30 days')
);

-- The overlap probe and the participant access query both filter by vehicle and
-- then by window. Vehicle first because it is the selective column.
CREATE INDEX IF NOT EXISTS idx_go_trips_vehicle_window
    ON go_trips (vehicle_id, starts_at, ends_at);

-- GET /api/trips as OWNER, newest first.
CREATE INDEX IF NOT EXISTS idx_go_trips_owner_created
    ON go_trips (owner_user_id, created_at DESC);

-- The sweeper's candidate scan. Partial over the trips that still have a
-- transition ahead of them: once both stamps are set a trip is finished with
-- the sweeper forever, and finished trips are the ones that accumulate.
CREATE INDEX IF NOT EXISTS idx_go_trips_unswept
    ON go_trips (starts_at, ends_at)
    WHERE started_notified_at IS NULL OR ended_notified_at IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- go_trip_participants
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS go_trip_participants (
    trip_id  TEXT NOT NULL REFERENCES go_trips (id) ON DELETE CASCADE,

    -- The person. Opaque cuid; the accepted share's accepted_by_user_id.
    user_id  TEXT NOT NULL,

    -- The share the person was picked FROM. Recorded because the participant
    -- picker's rows are shares, not users, and the wire contract's
    -- `participantId` IS the shareId — so the roster round-trips without the
    -- client having to hold a second identifier. It is NOT the access check:
    -- the access query re-joins go_vehicle_shares on (vehicle, user) and
    -- re-tests status/suspension, so a share that is revoked and re-granted
    -- under a new id does not resurrect or strand anybody.
    share_id TEXT NOT NULL,

    added_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Set when the participant LEAVES (DELETE …/participants/me) or when the
    -- owner removes them via PATCH. A tombstone rather than a DELETE so that
    -- "was Nabil ever on this trip?" stays answerable and so that re-adding is
    -- an UPDATE of one row rather than a second row for the same person.
    left_at  TIMESTAMPTZ,

    -- One membership per person per trip. Re-adding clears left_at in place.
    PRIMARY KEY (trip_id, user_id)
);

-- The ACCESS query's direction: given a user, which vehicles are they a live
-- participant on right now. Partial over live memberships because a departed
-- participant can never widen the access set and must not weigh on the query
-- that runs on every WebSocket handshake.
CREATE INDEX IF NOT EXISTS idx_go_trip_participants_user_live
    ON go_trip_participants (user_id, trip_id)
    WHERE left_at IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- go_trip_activity_tokens
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS go_trip_activity_tokens (
    trip_id             TEXT        NOT NULL REFERENCES go_trips (id) ON DELETE CASCADE,

    -- The party whose phone this token addresses. The OWNER may hold a row here
    -- too — the owner is included in the per-leg Activity by explicit product
    -- decision — so this is deliberately not constrained to participants.
    user_id             TEXT        NOT NULL,

    -- P1 CAPABILITY. Whoever holds this token together with the team's APNs
    -- signing key can START a Live Activity on that phone. Never logged beyond
    -- an 8-character prefix, never echoed in a response, never in an error.
    push_to_start_token TEXT        NOT NULL,

    -- Which APNs gateway the token belongs to. A development or TestFlight
    -- build mints a sandbox token, and pushing it to production is rejected as
    -- BadDeviceToken. Carried per-registration rather than read from the device
    -- registry for the same reason §7.21 gives: starting an Activity needs no
    -- notification permission, so the user may have no device row at all.
    sandbox             BOOLEAN     NOT NULL DEFAULT FALSE,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- UPSERT target. ActivityKit rotates the push-to-start token, so a
    -- re-registration REPLACES the value in place rather than accumulating.
    PRIMARY KEY (trip_id, user_id)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- go_trip_legs
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS go_trip_legs (
    id                    TEXT        PRIMARY KEY,
    trip_id               TEXT        NOT NULL REFERENCES go_trips (id) ON DELETE CASCADE,

    -- Denormalised from the trip so the leg detector's hot path — "does this
    -- vehicle have an open leg?" — needs no join. Opaque cuid, no FK (CG-DL-9).
    vehicle_id            TEXT        NOT NULL,

    -- Where the car said it was going when the leg began, AES-256-GCM sealed
    -- (P1 place name). NOT NULL: a leg is DEFINED as driving WITH a destination
    -- — a car pulling out of a driveway with no route set starts no leg — so
    -- there is no destinationless leg to represent.
    destination_name_enc  TEXT        NOT NULL,

    started_at            TIMESTAMPTZ NOT NULL,

    -- NULL while the leg is underway. Set when the car parks, clears its
    -- destination, arrives, or the trip's window closes underneath it.
    ended_at              TIMESTAMPTZ,

    -- TRUE only when there was actual ARRIVAL EVIDENCE — the internal/arrival
    -- detector's 80 m / 20 s dwell at the destination. A leg that ended because
    -- the driver changed their mind, parked short, or ran past the end of the
    -- window is `arrived = FALSE`, and the difference is load-bearing: the
    -- `trip_leg_arrived` push fires only on evidence, and the Live Activity's
    -- final content-state carries status `arrived` versus `completed`
    -- accordingly.
    arrived               BOOLEAN     NOT NULL DEFAULT FALSE,

    -- Per-leg push idempotency, same shape and same reasoning as go_trips'
    -- two stamps.
    started_notified_at   TIMESTAMPTZ,
    arrived_notified_at   TIMESTAMPTZ,

    -- When the push-to-start fan-out ran for this leg, and when the Activities
    -- were ended. Separate from the push stamps because they are separate
    -- deliveries with separate failure modes: an alert can succeed while a
    -- push-to-start fails, and each must retry without re-sending the other.
    activity_started_at   TIMESTAMPTZ,
    activity_ended_at     TIMESTAMPTZ,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- AT MOST ONE OPEN LEG PER TRIP, enforced in the schema rather than by the
-- detector's care. The detector is event-driven and its inputs can arrive
-- twice (a redelivered drive-start, two processes during a rolling deploy), and
-- a second open leg would produce a second Live Activity on every participant's
-- lock screen for the same journey.
CREATE UNIQUE INDEX IF NOT EXISTS idx_go_trip_legs_open_per_trip
    ON go_trip_legs (trip_id)
    WHERE ended_at IS NULL;

-- The detector's other direction: given a vehicle that just started driving,
-- is there an open leg to close or extend?
CREATE INDEX IF NOT EXISTS idx_go_trip_legs_vehicle_open
    ON go_trip_legs (vehicle_id)
    WHERE ended_at IS NULL;

-- GET /api/trips/{id} renders the current leg; the trip detail also counts.
CREATE INDEX IF NOT EXISTS idx_go_trip_legs_trip_started
    ON go_trip_legs (trip_id, started_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- go_live_activities gains a SECOND ANCHOR
-- ─────────────────────────────────────────────────────────────────────────────
--
-- A Live Activity row is keyed to the thing it is ABOUT. Until now that was
-- always a ride, so `ride_request_id` was NOT NULL. A trip leg is the second
-- kind of thing, and the two are mutually exclusive: an Activity describes one
-- ride OR one leg, never both and never neither.
--
-- THE `NOT NULL` DROP IS THE RISKY PART OF THIS MIGRATION and it is compensated
-- IN THE SAME STATEMENT by go_live_activities_one_anchor, which is STRICTER
-- than the constraint it replaces — the old schema permitted no bad state that
-- the new one permits. Every existing row satisfies it unchanged (ride set,
-- leg NULL), so the constraint is added VALID with no scan concern beyond the
-- one-time check.
--
-- The alternative — a separate go_trip_leg_activities table — was considered
-- and rejected: the ETA ticker, the held-end machinery, the token rotation
-- upsert and the 24-hour reaper would all have needed a second implementation,
-- and four copies of the alert-on-update-then-end rule is how one of them ends
-- up wrong.

ALTER TABLE go_live_activities
    ALTER COLUMN ride_request_id DROP NOT NULL;

ALTER TABLE go_live_activities
    ADD COLUMN IF NOT EXISTS trip_leg_id TEXT
        REFERENCES go_trip_legs (id) ON DELETE CASCADE;

ALTER TABLE go_live_activities
    DROP CONSTRAINT IF EXISTS go_live_activities_one_anchor;

ALTER TABLE go_live_activities
    ADD CONSTRAINT go_live_activities_one_anchor
        CHECK ((ride_request_id IS NOT NULL) <> (trip_leg_id IS NOT NULL));

-- The ride path's uniqueness came from a table constraint over
-- (ride_request_id, user_id). That constraint still holds and is untouched:
-- Postgres UNIQUE treats NULLs as distinct, so the trip rows it now also
-- contains (ride_request_id NULL) never collide with each other through it and
-- never weaken it for rides.
--
-- The leg side needs its own, and it must be PARTIAL so the ride rows (with
-- trip_leg_id NULL) are excluded rather than merely tolerated.
CREATE UNIQUE INDEX IF NOT EXISTS idx_go_live_activities_leg_user
    ON go_live_activities (trip_leg_id, user_id)
    WHERE trip_leg_id IS NOT NULL;

-- The trip ticker's candidate scan, mirroring idx_go_live_activities_live for
-- the leg anchor.
CREATE INDEX IF NOT EXISTS idx_go_live_activities_leg_live
    ON go_live_activities (trip_leg_id)
    WHERE trip_leg_id IS NOT NULL AND ended_at IS NULL;
