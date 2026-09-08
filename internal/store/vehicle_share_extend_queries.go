package store

// SQL for MYR-609 — extending an ACCEPTED grant onto a second car the same
// owner owns (rest-api.md §7.5.8). Kept beside the statements it is a sibling
// of rather than inside vehicle_share_queries.go, which is at the 300-line
// limit; the two invariants stated at the top of that file govern these
// statements too:
//
//  1. Every mutation carries `owner_user_id = $n` IN THE STATEMENT. The
//     handler's ownership check on the PATH vehicle produces the good error
//     message; these predicates are what actually prevent one person extending
//     another person's grant.
//
//  2. The read that decides is a locking SELECT inside the same transaction as
//     the write, so a revoke or a suspend landing between them cannot produce a
//     grant copied from a row that no longer conveys anything.

// queryLockSourceAcceptedShare selects and LOCKS the grant being extended.
//
// FOUR PREDICATES, and each one is a refusal the endpoint owes:
//
//   - `id = $1` — the row the caller named.
//   - `owner_user_id = $2` — the caller GRANTED it. Somebody else's grant is
//     not a source, and a caller who merely holds a grant cannot re-share the
//     car it is on (rest-api.md §7.5: sharing never grants re-sharing).
//   - `status = 'accepted'` — a PENDING invite has no grantee to copy. There is
//     nobody to extend the share to; the code has not been redeemed and may
//     never be. A revoked tombstone is excluded by the same predicate.
//   - `accepted_by_user_id` non-empty — belt and braces on the column an
//     accepted row always has. The whole feature is "give THIS PERSON the other
//     car too", so a row that cannot name the person is not extendable, and
//     without this predicate a NULL would be copied into the new row's grantee
//     column and produce a grant addressed at nobody.
//
// Every miss returns the SAME zero rows, which the repository turns into ONE
// ErrShareNotFound. A caller learns nothing about which predicate they failed —
// the non-oracle rule §7.5.3 and §7.5.7 already hold for invite ids.
//
// A SUSPENDED SOURCE IS SELECTED HERE AND REFUSED IN GO, deliberately, and the
// split is the point. Suspension is not a reason the source is unreadable — it
// is the owner's own explicit pause on a relationship they can see on their own
// §7.5.2 listing — so collapsing it into the 404 set would answer "that share
// is not extendable by you" to somebody who is looking straight at it. It is
// answered instead with a 409 that says what to do (`ErrShareSourceSuspended`,
// "restore it in Share first"), which needs the row to have been read.
//
// The earlier cut copied the pause forward instead. That produced a grant born
// suspended: a row nobody would know to un-pause, on a car the owner's own
// screen would show as shared, invisible to the grantee for the same reason a
// revoked row is — and it broke the invariant `queryAcceptSharesByID` asserts
// in the other direction (`suspended_at = NULL`, "a freshly accepted grant must
// never be born paused"). A pause is a state to lift, not a state to propagate.
//
// FOR UPDATE holds the source for the length of the transaction, so a
// concurrent revoke of the source serialises against it rather than racing the
// insert that copies it.
const queryLockSourceAcceptedShare = `
SELECT vehicle_id, label, permission, allow_rides, suspended_at, accepted_by_user_id
FROM go_vehicle_shares
WHERE id = $1 AND owner_user_id = $2 AND status = 'accepted'
  AND COALESCE(accepted_by_user_id, '') <> ''
FOR UPDATE`

// queryExtendTargetBlock answers, in ONE round trip, the only question left
// once the source is locked and the target is known to be the caller's: is
// there anything on the TARGET car that forbids this grant?
//
// It returns two nullable columns, and the repository reads them in order.
//
// COLUMN 1 — the LIVE row this grantee already holds on the target, projected
// as the refusal it produces:
//
//   - `already_shared` — a live, unpaused grant. The 409 `already_shared` the
//     contract tells clients to render as SUCCESS, because the thing the caller
//     asked for is already true.
//   - `target_suspended` — a live grant the owner has PAUSED. This is NOT the
//     already-shared case and must never be reported as one: the client would
//     render "already has this car" for somebody who currently has nothing,
//     and the owner would be told a paused person is fine. It is its own 409,
//     naming the pause.
//
// `status <> 'revoked'` rather than `status = 'accepted'`, deliberately: a
// tombstone is gone and must not block a re-share by EXISTING (that is what
// column 2 is for, and it blocks on authorship, not on existence), while
// anything still live counts. In practice only an accepted row can match,
// because a pending row's `accepted_by_user_id` is NULL — which is itself the
// honest answer to "does an outstanding invite block an extend?": it does not,
// because nobody holds it yet.
//
// COLUMN 2 — the AUTHOR of the newest tombstone for this (car, grantee) pair,
// `revoked_by` from migration 0051. It is what stops an extend from silently
// undoing a §7.5.7 LEAVE: the grantee walked away from this car, nobody told
// them anything, and re-granting it on the owner's button press would make the
// one exit a grantee has reversible by the party they were leaving. Only a
// 'grantee' value blocks; 'owner' does not (an owner re-sharing a car they
// themselves un-shared is the ordinary case this endpoint exists for), and NULL
// does not (a tombstone written before 0051, whose author was never recorded —
// see the migration for why unknown fails open).
//
// NEWEST WINS, and only the newest is consulted. A grantee who left and was
// later re-invited and re-accepted has a live row, which column 1 catches; a
// grantee who left, was re-invited, and had that invite revoked by the owner
// has an owner tombstone on top and is extendable again. Reading the whole
// history instead would make a single old leave permanent.
//
// `revoked_at DESC NULLS LAST, id DESC` because `revoked_at` is stamped by
// every writer of a tombstone but is nullable in the schema, and two tombstones
// stamped in the same statement need a deterministic tiebreak.
const queryExtendTargetBlock = `
SELECT
	(SELECT CASE WHEN live.suspended_at IS NULL THEN 'already_shared' ELSE 'target_suspended' END
	 FROM go_vehicle_shares live
	 WHERE live.vehicle_id = $1 AND live.accepted_by_user_id = $2 AND live.status <> 'revoked'
	 LIMIT 1) AS live_block,
	(SELECT tomb.revoked_by
	 FROM go_vehicle_shares tomb
	 WHERE tomb.vehicle_id = $1 AND tomb.accepted_by_user_id = $2 AND tomb.status = 'revoked'
	 ORDER BY tomb.revoked_at DESC NULLS LAST, tomb.id DESC
	 LIMIT 1) AS newest_tombstone_author`

// queryInsertExtendedShare writes the extended grant: a row born ACCEPTED.
//
// IT IS THE ONLY STATEMENT IN THIS PACKAGE THAT INSERTS AN ACCEPTED ROW.
// Everywhere else a grant becomes accepted by UPDATE, at redemption, because
// somebody presented a code. Here the consent is already on file — this same
// grantee accepted a share from this same owner on another of their cars — and
// the row records that: `accepted_at = NOW()` is the instant the OWNER extended
// it, which is the moment the access actually began.
//
// `code` AND `expires_at` ARE NOT WRITTEN AT ALL, and land NULL (migration
// 0052, which dropped both NOT NULLs and replaced them with a CHECK that
// requires them only on a PENDING row). There is no credential here, because
// nothing about this row is redeemable: minting a real code and stamping an
// already-lapsed expiry — what the first cut did to satisfy the old NOT NULL —
// put a live-looking bearer credential in the table and rested its deadness on
// three unrelated predicates continuing to agree. A NULL is unreachable
// without an argument.
//
// `suspended_at` IS NOT WRITTEN EITHER, and lands NULL. A freshly written grant
// is never born paused — the same invariant `queryAcceptSharesByID` asserts
// explicitly on the redeem path, for the same reason: nobody would know to
// un-pause it. A suspended SOURCE does not reach this statement; it is refused
// before the write (see queryLockSourceAcceptedShare).
//
// `updated_at = NOW()` for the MYR-451 reason: this statement DECIDES a
// capability set (it copies `allow_rides`), and the column exists to date
// exactly that. A grant that arrived by extension must be placeable in time
// beside one that arrived by redemption.
//
// The four copied values ($4 label, $5 permission, $6 allow_rides, $7
// accepted_by_user_id) are read from the LOCKED source row in the same
// transaction, so what is copied is what the source held at the instant of the
// write.
const queryInsertExtendedShare = `
INSERT INTO go_vehicle_shares
	(id, vehicle_id, owner_user_id, label, permission, allow_rides,
	 accepted_by_user_id, status, created_at, accepted_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'accepted', NOW(), NOW(), NOW())
RETURNING` + shareColumns
