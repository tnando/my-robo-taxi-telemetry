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
// A SUSPENDED source is deliberately still selectable. Suspension is the
// owner's own reversible pause, this caller IS that owner, and the extend
// copies the pause forward (see queryInsertExtendedShare): refusing here would
// force an owner to un-pause somebody in order to add them to a second car,
// which is the opposite of what a pause means.
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

// queryLiveShareForGrantee probes whether the grantee already holds a LIVE row
// on the target vehicle — the 409 `already_shared` conflict.
//
// `status <> 'revoked'` rather than `status = 'accepted'`, deliberately: a
// tombstone is gone and must not block a re-share (that is the whole point of a
// revoke being reversible only through a fresh grant), while anything still
// live counts. In practice only an accepted row can match, because a pending
// row's `accepted_by_user_id` is NULL — which is itself the honest answer to
// "does an outstanding invite block an extend?": it does not, because nobody
// holds it yet, and redeeming it afterwards hits the partial-unique index and
// is refused there.
//
// It is a PROBE, not the enforcement. `uq_go_vehicle_shares_accepted_grant`
// (migration 0020) is what actually forbids the second accepted grant; this
// statement exists to turn that 23505 into a 409 the caller can read BEFORE any
// work is done. The insert still maps the unique violation, because two
// concurrent extends can both pass this probe.
const queryLiveShareForGrantee = `
SELECT 1 FROM go_vehicle_shares
WHERE vehicle_id = $1 AND accepted_by_user_id = $2 AND status <> 'revoked'
LIMIT 1`

// queryInsertExtendedShare writes the extended grant: a row born ACCEPTED.
//
// IT IS THE ONLY STATEMENT IN THIS PACKAGE THAT INSERTS AN ACCEPTED ROW.
// Everywhere else a grant becomes accepted by UPDATE, at redemption, because
// somebody presented a code. Here the consent is already on file — this same
// grantee accepted a share from this same owner on another of their cars — and
// the row records that: `accepted_at = NOW()` is the instant the OWNER extended
// it, which is the moment the access actually began.
//
// `code` IS A FRESH, ALREADY-DEAD CREDENTIAL, and the schema is why the row has
// one at all: `code TEXT NOT NULL` (migration 0020). The value is minted
// through the same collision probe every other code takes, so it cannot shadow
// a live pending invite, and it is unreachable three times over — the redeem
// statement requires `status = 'pending'`, `expires_at` is stamped ALREADY
// LAPSED (`NOW()`, against a strict `expires_at > NOW()` predicate) rather than
// seven days out, and `shareColumns` blanks `code` in SQL for any row that is
// not pending, so it never reaches a response and nobody can present what they
// cannot read. The alternative — an empty string — would
// collide with the projection's own "no code" sentinel and make a real absence
// indistinguishable from a stored blank.
//
// `updated_at = NOW()` for the MYR-451 reason: this statement DECIDES a
// capability set (it copies `allow_rides`), and the column exists to date
// exactly that. A grant that arrived by extension must be placeable in time
// beside one that arrived by redemption.
//
// The five copied values ($4 label, $5 permission, $7 allow_rides, $8
// suspended_at, $9 accepted_by_user_id) are read from the LOCKED source row in
// the same transaction, so what is copied is what the source held at the
// instant of the write.
const queryInsertExtendedShare = `
INSERT INTO go_vehicle_shares
	(id, vehicle_id, owner_user_id, label, permission, code, allow_rides,
	 suspended_at, accepted_by_user_id, status, created_at, expires_at,
	 accepted_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'accepted', NOW(), NOW(),
	 NOW(), NOW())
RETURNING` + shareColumns
