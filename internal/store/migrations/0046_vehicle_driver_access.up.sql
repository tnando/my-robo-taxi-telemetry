-- 0046_vehicle_driver_access.up.sql
--
-- MYR-599: go_vehicle_driver_access — the record that a car was linked by
-- someone Tesla says is a DRIVER of it rather than its OWNER, and whether that
-- person has acknowledged that the owner approved adding it.
--
-- ── THE PROBLEM THIS SERVES ─────────────────────────────────────────────────
--
-- MYR-257 finding 3 put an ownership filter in the post-link provisioning hook:
-- every Fleet-API vehicle whose `access_type` is not `OWNER` was skipped, with
-- an `owner_vehicle_skipped reason=not_owner` audit line and nothing else. On
-- 2026-09-05 a tester ran "Add another Tesla" for a car he drives on somebody
-- else's Tesla account. The OAuth link completed, the token was stored, he
-- paired the virtual key — and no "Vehicle" row was ever created, so the app
-- had nothing to show and nothing to explain. The filter was silent by design;
-- silence was the bug.
--
-- The client's ruling (Thomas, 2026-09-05) is that a driver MAY add the car,
-- with a pop-up in which they acknowledge that the owner approved it. So the
-- filter is replaced by CONSENT: driver-access cars ARE provisioned, and
-- NOTHING is pushed at the car until the driver acknowledges.
--
-- ── WHAT THE ROW IS, AND WHAT IT IS NOT ─────────────────────────────────────
--
-- It is EVIDENCE, not a permission. The platform cannot verify with Tesla that
-- an owner approved anything — Tesla's API exposes no such fact — so what is
-- recorded is exactly what the platform actually knows: this person, at this
-- instant, was shown this version of this text and said yes. That is the thing
-- we would point to if an owner ever complains, and it is the whole reason
-- `acknowledgment_version` exists rather than a bare boolean: copy changes, and
-- an acknowledgment that cannot name what was acknowledged is worth nothing.
--
-- ROW PRESENCE IS THE CLAIM "TESLA CALLS THIS PERSON A DRIVER OF THIS CAR".
-- Absence means owner access, which is why the §7.0 / §7.1 wire field
-- `teslaAccessType` reads an absent row as `"owner"` and why a server predating
-- this migration is contract-compatible by omission. An OWNER re-link of a car
-- that carries a driver row DELETES the row (access was upgraded on Tesla's
-- side), so the table cannot outlive the fact it describes.
--
-- ── NO FK, AND THIS IS A DELIBERATE DEPARTURE FROM THE ISSUE TEXT ───────────
--
-- MYR-599's server design asks for `vehicle_id TEXT PRIMARY KEY REFERENCES
-- "Vehicle"("id") ON DELETE CASCADE`. That is UNWRITABLE here. CG-DL-9
-- (docs/contracts/data-lifecycle.md §7, docs/architecture/migrations.md §4.2)
-- forbids any file under internal/store/migrations/ from NAMING a Prisma-owned
-- table at all — the CI gate greps for the identifier — so the FK would fail the
-- build before it could fail a deploy. Migrations 0031 (go_fleet_config_attempts)
-- and 0044 (go_vehicle_telemetry_suspensions) key off `"Vehicle"."id"` the same
-- way and say the same thing.
--
-- The CONSEQUENCE is that nothing cascades, so every row has to be named where
-- it must go, and it is named in two places:
--
--   * The per-vehicle teardown (store.RemoveVehicle) deletes it in the SAME
--     transaction as the vehicle, beside the MYR-592 suspension episode and the
--     MYR-593 fleet-config schedule.
--   * The account-deletion sequence deletes it as step 8f, AFTER step 3, for
--     exactly the reason step 8e (the MYR-596 tombstones) runs after step 3:
--     the teardown is what would otherwise leave rows behind.
--
-- ── CLASSIFICATION ──────────────────────────────────────────────────────────
--
-- P0 in full (docs/contracts/data-classification.md §1.24). Two opaque cuids,
-- Tesla's own access-type token ("OWNER"/"DRIVER" — a role name, not a
-- credential and not a person), a version STRING that names a document, and two
-- timestamps about a platform action. No VIN, no token, no coordinate, no name.
-- `tesla_access_type` is the one value that reaches a wire, folded down to the
-- two-member enum `teslaAccessType` on BOTH roles (contracts v0.39.0).

CREATE TABLE IF NOT EXISTS go_vehicle_driver_access (
    -- The car. Opaque Prisma cuid, no FK (CG-DL-9 — see the header).
    --
    -- PRIMARY KEY rather than a surrogate id with a UNIQUE on top: a vehicle is
    -- linked by exactly one account in this platform (UpsertOwnedVehicle refuses
    -- a cross-user teslaVehicleId), so a second row would not be a second fact.
    -- It also gives the reconciler's NOT EXISTS gate an index for free.
    vehicle_id             TEXT PRIMARY KEY,

    -- The person who linked it — the same subject the "Vehicle" row is filed
    -- under, carried here so the account-deletion sequence can reach these rows
    -- by user id without joining a table that may already be gone.
    user_id                TEXT NOT NULL,

    -- Tesla's `access_type` VERBATIM, as the Fleet API vehicles listing spelled
    -- it: "DRIVER" today, and whatever Tesla adds tomorrow. Stored raw rather
    -- than folded to a boolean because the raw token is the only thing that can
    -- answer "what did Tesla actually say?" months later, and because Tesla has
    -- shipped an EMPTY access_type on older responses — which this platform
    -- treats as driver (fail closed: an unknown access level must not be
    -- promoted to ownership), and which is stored as '' rather than invented.
    --
    -- NOT NULL with no default: a row exists because a listing said something,
    -- so there is no state a NULL could describe.
    tesla_access_type      TEXT NOT NULL,

    -- When the driver acknowledged that the owner approved adding this car.
    -- NULL means NOT YET, and NULL is the gate: every config-push path in the
    -- server refuses a vehicle with a row whose acknowledged_at IS NULL.
    --
    -- Nullable rather than an `acknowledged BOOLEAN` for the reason 0044's
    -- warned_at is: the instant answers the operator (and, one day, the legal)
    -- question that a boolean cannot.
    acknowledged_at        TIMESTAMPTZ,

    -- WHICH TEXT they agreed to — the version id contracts publishes alongside
    -- the copy (`owner-approval-v1`, docs/owner-approval-acknowledgment.md in
    -- the contracts repo). NULL exactly while acknowledged_at is NULL.
    --
    -- The VERSION ONLY, never the rendered copy: the text is a published
    -- document with a stable id, and storing a per-row copy of it would be a
    -- large mutable duplicate of something that must not vary per row.
    acknowledgment_version TEXT,

    -- When the driver-access row was recorded — i.e. when the car was
    -- provisioned by someone Tesla calls a driver. This is the `since` the
    -- `awaiting_owner_acknowledgment` setup state carries on the wire, which is
    -- why it is NOT NULL: a state with no start renders as "for 2025 years".
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Serves the account-deletion step and the ops listings, both of which reach
-- these rows by person rather than by car. Not unique: one driver may hold
-- driver access to several cars.
CREATE INDEX IF NOT EXISTS idx_go_vehicle_driver_access_user
    ON go_vehicle_driver_access (user_id);

-- Serves the push gate. Partial, over the UNACKNOWLEDGED rows only, because
-- those are the ones every gate asks about and they are the minority that
-- shrinks over time: a row is created unacknowledged and, in the ordinary
-- course, acknowledged within a minute of the driver seeing the sheet. The
-- acknowledged rows stay in the table forever as the evidence they are, and
-- this index deliberately does not carry them.
CREATE INDEX IF NOT EXISTS idx_go_vehicle_driver_access_pending
    ON go_vehicle_driver_access (vehicle_id)
    WHERE acknowledged_at IS NULL;
