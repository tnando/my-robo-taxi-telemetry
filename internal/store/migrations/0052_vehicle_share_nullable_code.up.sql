-- 0052_vehicle_share_nullable_code.up.sql
--
-- MYR-609: `code` and `expires_at` become NULLABLE, and a CHECK makes them
-- REQUIRED exactly where they mean something.
--
-- WHAT WAS WRONG. Migration 0020 declared `code TEXT NOT NULL` and
-- `expires_at TIMESTAMPTZ NOT NULL` because every row was born pending and a
-- pending row is a credential with a deadline. §7.5.8 extend writes a row born
-- ACCEPTED — nobody redeems anything — so it had nothing to put in either
-- column, and the first cut minted a REAL 6-character code and stamped an
-- already-lapsed expiry just to satisfy the constraint.
--
-- That is a dead credential in a live row, and "dead" rested on three separate
-- predicates all continuing to hold: the redeem statement requiring
-- `status = 'pending'`, `expires_at > NOW()` refusing the lapsed stamp, and the
-- projection blanking `code` off any non-pending row. Every one of those is
-- true today and any one of them is a line somebody could change for an
-- unrelated reason. A credential that is unreachable by argument is a
-- credential; a column that is NULL is not one.
--
-- The mint round trip goes with it: extending no longer draws from the code
-- space or probes it for collisions, so it cannot shadow a live pending invite
-- even in principle.
--
-- THE CHECK IS THE INVARIANT THE NOT NULL USED TO CARRY, stated where it is
-- actually true: a PENDING row must have both (it is a code with a deadline);
-- an accepted or revoked row may have neither. It is written as an implication
-- rather than two conditional constraints so a single statement rejects the
-- only shape that is nonsense — a pending row nobody can redeem.
--
-- No backfill and no data change: every existing row is either pending with
-- both values or non-pending with values that are already inert. The CHECK
-- validates against the whole table on the way in, which is what proves that.
--
-- READ PATHS ARE UNAFFECTED BY CONSTRUCTION. `queryLockPendingByCode` and
-- `queryAcceptedSharesByCodeAndUser` both match `code = $1`, which is never
-- true for NULL; `expires_at > NOW()` is likewise never true for NULL. So a
-- row with NULLs is invisible to every redeem path without any of them
-- learning a new predicate. `shareColumns` already blanks `code` for non-
-- pending rows, and `scanShare` reads `expires_at` through a nullable local.

ALTER TABLE go_vehicle_shares
    ALTER COLUMN code       DROP NOT NULL,
    ALTER COLUMN expires_at DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'go_vehicle_shares_pending_credential_check'
    ) THEN
        ALTER TABLE go_vehicle_shares
            ADD CONSTRAINT go_vehicle_shares_pending_credential_check
            CHECK (status <> 'pending' OR (code IS NOT NULL AND expires_at IS NOT NULL));
    END IF;
END
$$;
