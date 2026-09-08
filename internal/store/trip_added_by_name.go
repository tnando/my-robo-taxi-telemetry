package store

// The name of the person who ADDED a roster entry to a trip (MYR-618).
//
// Split into its own file for the reason vehicle_share_accepted_name.go gives
// for its sibling: it is one expression, but the argument for why it is yet
// another copy of the confirmed-name ladder rather than a reuse of an existing
// one is not something to bury in the middle of an authorization-relevant
// statement set.
//
// ── WHY ANOTHER COPY ────────────────────────────────────────────────────────
//
// This is the platform's SIXTH spelling of the same three-rung ladder, and the
// FOURTH that carries MYR-583's confirmation gate. The full inventory, because
// two different counts are quoted around this code and each is right about a
// different set:
//
//  1. ownerNameLadderExpr        `"Vehicle"."userId"`      GATED
//  2. acceptedByNameExpr         `accepted_by_user_id`     GATED
//  3. queryTripOwnerFirstName    `go_trips.owner_user_id`  GATED (inline probe)
//  4. requesterIdentitySelect    the ride surfaces         ungated, deliberately
//  5. queryOwnerFirstNameSources the redeem screen         ungated, deliberately
//  6. addedByNameExpr            `p.added_by_user_id`      GATED  ← this one
//
// So: SIXTH of six ladders, FOURTH of four gated ones. owner_name_test.go's
// TestConfirmationGateIsSharedAndScoped counts the gated set (hence "fourth")
// and TestEveryNameLadderTrimsEveryRung counts all six.
//
// Every one of them exists because the embedding statements are `const`: a Go
// constant cannot take the key column as a parameter, and turning these into
// runtime-formatted SQL to save the copies would put string concatenation next
// to statements that decide who may see a car. The duplication is CHECKED
// rather than trusted — TestConfirmationGateIsSharedAndScoped asserts this one
// carries the confirmation probe, and TestEveryNameLadderTrimsEveryRung asserts
// every rung of all six is TRIM-guarded, both in owner_name_test.go.
//
// ── SAME GATE, SAME REDUCTION, SAME ABSENCE ─────────────────────────────────
//
// MYR-583's confirmation gate applies unchanged: a person who has not been
// through the naming prompt resolves NULL, and the wire spells that as a null
// `addedByName` rather than as a placeholder. The FULL name is selected here
// and reduced to its first token in Go by the shared `ownerFirstNameToken`, so
// this surface shortens a name exactly the way every other one does.
//
// `added_by_user_id` IS NULLABLE and that is free correctness rather than a
// gap: every roster row written before migration 0060 has no adder recorded, a
// scalar subselect compared against NULL matches nothing, the confirmation
// EXISTS is FALSE, and the name resolves absent — which is precisely how those
// rows should render, because nobody observed who added them.
// addedByNameConfirmedExpr is MYR-583's confirmation probe keyed on the ADDER.
// Named rather than inlined so TestConfirmationGateIsSharedAndScoped can assert
// that the ladder below acquires its gate from one place, the way its three
// siblings do.
const addedByNameConfirmedExpr = `EXISTS (
			SELECT 1 FROM go_profile_name_confirmations pnc
			WHERE pnc.user_id = p.added_by_user_id
		)`

const addedByNameExpr = `CASE WHEN ` + addedByNameConfirmedExpr + ` THEN COALESCE(
		NULLIF(TRIM((SELECT u."name" FROM "User" u WHERE u."id" = p.added_by_user_id)), ''),
		NULLIF(TRIM((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = p.added_by_user_id ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."name" FROM go_users g WHERE g.id = p.added_by_user_id)), '')
	) END AS added_by_name`
