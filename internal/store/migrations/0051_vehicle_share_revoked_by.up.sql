-- 0051_vehicle_share_revoked_by.up.sql
--
-- MYR-609: record WHO ended a share on the tombstone.
--
-- WHY A REVOKE NEEDED AN AUTHOR. Since MYR-184 a revoked row has recorded THAT
-- access ended (`status = 'revoked'`, `revoked_at` stamped) and nothing about
-- who ended it. That was enough while the only reader was the audit trail, and
-- it stopped being enough the moment §7.5.8 extend arrived: an owner extending
-- a grant onto a car the grantee had DELIBERATELY LEFT (§7.5.7) would hand back
-- the access that person walked away from, with the grantee performing no act
-- and receiving no notification. The leave is the one exit a grantee has, and
-- an endpoint that silently undoes it is not an exit.
--
-- So the tombstone now names its author, and §7.5.8 refuses when the newest
-- tombstone for (owner, target vehicle, grantee) is grantee-initiated. An
-- OWNER-initiated tombstone does not block: an owner re-sharing a car they
-- themselves un-shared is the ordinary case the endpoint exists for.
--
-- TWO VALUES AND NO MORE. 'owner' covers every write the owner's side makes —
-- the §7.5.3 revoke, the vehicle-offboarding sweep, and the redeem path's
-- SUPERSEDED tombstone (a pending row for a car the redeemer already holds a
-- live grant on, retired so the REST of a multi-car code can still accept).
-- 'grantee' covers the §7.5.7 leave and the grantee's own account deletion.
-- The column is a CHECKed enum rather than free text because the extend gate
-- branches on it: an unconstrained value would be a third state nobody decided.
--
-- NULL IS "UNKNOWN", AND IT IS THE PRE-EXISTING TAIL. Every tombstone written
-- before this migration carries NULL and there is no way to recover its author
-- — the information was never captured. NULL therefore DOES NOT BLOCK an
-- extend, which is the fail-open direction and is deliberate: blocking on
-- unknown would refuse every extend against a car with any historical
-- tombstone, which is most of them, for a leave that probably never happened.
-- The window closes on its own as old tombstones stop being the newest one.
--
-- `revoked_reason` is the tombstone's own explanation, written only where
-- "who" is not the whole story. Today exactly one value reaches it —
-- 'superseded', the redeem path retiring a pending row it cannot accept — and
-- every other tombstone leaves it NULL. It is P0: a fixed vocabulary, no
-- identifier, no free text (data-classification.md §1.15).
--
-- Naming convention (CG-DL-9): Go-owned table, snake_case columns, no sibling
-- Prisma relation named anywhere in this file.

ALTER TABLE go_vehicle_shares
    ADD COLUMN IF NOT EXISTS revoked_by     TEXT,
    ADD COLUMN IF NOT EXISTS revoked_reason TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'go_vehicle_shares_revoked_by_check'
    ) THEN
        ALTER TABLE go_vehicle_shares
            ADD CONSTRAINT go_vehicle_shares_revoked_by_check
            CHECK (revoked_by IS NULL OR revoked_by IN ('owner', 'grantee'));
    END IF;
END
$$;

-- The extend gate's read: "the newest tombstone for this (vehicle, grantee)".
-- Partial on the tombstones because that is the only status it ever asks
-- about, and ordered by the stamp it picks the newest by.
CREATE INDEX IF NOT EXISTS idx_go_vehicle_shares_revoked_grantee
    ON go_vehicle_shares (vehicle_id, accepted_by_user_id, revoked_at DESC)
    WHERE status = 'revoked';
