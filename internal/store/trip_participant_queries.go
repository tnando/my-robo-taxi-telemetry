package store

// EVERY STATEMENT THAT READS OR WRITES go_trip_participants, in one file.
//
// ⚠ IT INHERITS trip_queries.go's DELIBERATE EXCEPTION TO THE 300-LINE FILE CAP
// (CLAUDE.md "File Rules"), and claims it explicitly rather than by proximity.
// The parent file's argument applies verbatim: the cap exists so a file has one
// subject, this file's subject is "the membership statements, together", and the
// invariants are properties OF THE SET. Splitting it further — the upsert here,
// the departures there, the picker somewhere else — is precisely the split by
// operation the parent header argues against, and it would put the statement
// that decides whether an owner's removal is reversible in a different file from
// the one that reverses it.
//
// A SIBLING OF trip_queries.go RATHER THAN A DEPARTURE FROM IT. That file's
// header states the rule this one obeys: the statements are kept together so
// the authorization-relevant set can be read at once, and the three invariants
// it declares — the owner-scoped mutation predicate, the one spelling of the
// window, and the live-share re-join — hold ACROSS BOTH FILES AS ONE. Read the
// two as a single file that is stored in two, and read that header first: it is
// where the invariants are stated and it is not repeated here.
//
// The split is by RELATION, not by operation. Splitting by operation is exactly
// what trip_queries.go's header argues against — reads here, writes there, and
// invariant 2 becomes four locally-correct spellings of one window predicate.
// This seam is different: `go_trip_participants` is one table with one whole
// statement set, and every predicate in it is about membership. It moved here
// in the MYR-618 review round, when that set grew past the point where the
// parent file could be read in one sitting.
//
// WHAT IS HERE: the roster upsert (create, the owner's add and a participant's
// add all reach it), the share-id resolver in front of it, the owner-removal
// gate, the owner's remove, the participant's own leave, the revoked-share
// cascade, the live-roster read, §7.30.11's picker, the cheap role probe and
// the role expression that probe uses.
//
// WHAT IS NOT: anything about the WINDOW itself — the trip row, its overlap
// probe, the status filter, the legs, the drives and the activity tokens are
// all still in trip_queries.go, and so is `tripRoleExpr`, which belongs beside
// the reads that use it.

// queryAcceptedShareParticipants resolves the requested share ids to the people
// behind them, KEEPING ONLY the ones that are a live accepted grant on THIS
// vehicle.
//
// The filtering happens here rather than in Go, and the caller compares COUNTS
// rather than inspecting which id fell out, because the create endpoint must
// answer one thing for "no such share", "a share on a different car", "a share
// that was never accepted" and "a suspended share". A loop that reported the
// first failing id would be an oracle for other people's share ids.
const queryAcceptedShareParticipants = `
SELECT id, accepted_by_user_id
FROM go_vehicle_shares
WHERE vehicle_id = $1
  AND id = ANY($2::text[])
  AND status = 'accepted' AND suspended_at IS NULL
  AND accepted_by_user_id IS NOT NULL AND accepted_by_user_id <> ''`

// queryUpsertTripParticipant adds a person, or REVIVES a membership they had
// left. `left_at = NULL` in the DO UPDATE is the revival; `added_at` is
// deliberately NOT refreshed, because it answers "when did this person first
// join this trip" and a re-add should not erase that.
//
// `added_by_user_id` ($4) is the ACTOR — the owner, or since MYR-618 a live
// participant. THE DO UPDATE ARM PRESERVES IT WHEN THE ROW IS ALREADY LIVE and
// overwrites it only on a genuine revival, and the asymmetry is the whole
// attribution rule: re-sending somebody who is already on the trip is a no-op
// (§7.30.4), so it must not be able to rewrite who put them there — otherwise
// any participant could claim credit for the owner's own additions by adding
// them a second time. `go_trip_participants.left_at` inside DO UPDATE is the
// EXISTING row's value, which is what makes that distinction expressible.
//
// `removed_by_owner = FALSE` (migration 0061) CLEARS AN OWNER'S REMOVAL, and it
// is unconditional here because the only caller that may reach a removed row is
// the owner: addAndAuditParticipants refuses a PARTICIPANT's add of an
// owner-removed person before this statement runs. An owner re-adding somebody
// is the act that undoes their own removal, and it is the only act that may.
// On every other row the assignment is a no-op — a live row's marker is already
// FALSE, because the only way to set it is the owner's remove and the only way
// to leave it set is to stay removed.
const queryUpsertTripParticipant = `
INSERT INTO go_trip_participants (trip_id, user_id, share_id, added_by_user_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (trip_id, user_id) DO UPDATE
SET left_at = NULL,
    removed_by_owner = FALSE,
    share_id = EXCLUDED.share_id,
    added_by_user_id = CASE
        WHEN go_trip_participants.left_at IS NULL THEN go_trip_participants.added_by_user_id
        ELSE EXCLUDED.added_by_user_id
    END`

// queryTripOwnerRemovedUserIDs is the MYR-618 review round's gate: of the users
// being added, which ones did the trip's OWNER remove?
//
// Read inside the roster transaction, and only when the actor is NOT the owner
// — an owner add is what clears the marker, so asking would be asking whether
// they may undo their own decision.
//
// It returns USER IDS RATHER THAN SHARE IDS on purpose: the refusal that
// follows is deliberately unspecific about WHICH person (the same non-oracle
// reasoning `participant_not_shared` is built on), so the caller needs only to
// know that the set is non-empty. The projection exists so the statement can
// stay a plain `= ANY($2)` rather than an EXISTS the caller cannot read.
const queryTripOwnerRemovedUserIDs = `
SELECT user_id FROM go_trip_participants
WHERE trip_id = $1
  AND user_id = ANY($2::text[])
  AND removed_by_owner
  AND left_at IS NOT NULL`

// queryRemoveTripParticipant is the OWNER's §7.30.4 remove, keyed on the SHARE
// id — which is what the wire calls `participantId`.
//
// ⚠ IT IS THE ONE STATEMENT THAT SETS `removed_by_owner` (migration 0061), and
// that is what makes an owner's removal STICK. Without the marker the upsert's
// revival arm would let any other participant undo it by re-sending the same
// share id, which would make the one verb MYR-618 kept owner-only the one verb
// a participant could reverse.
//
// Idempotent by the `left_at IS NULL` guard, so a double-tap updates zero rows
// and the marker already written by the first call stands.
//
// It lived inline in trip_repo_write.go until the review round; it is here
// because this file's whole premise is that the authorization-relevant
// statement set can be read in one place, and a statement that decides whether
// a removal is reversible is squarely in that set.
const queryRemoveTripParticipant = `
UPDATE go_trip_participants
SET left_at = NOW(), removed_by_owner = TRUE
WHERE trip_id = $1 AND share_id = $2 AND left_at IS NULL`

// queryTripLiveParticipantUserIDs is the roster's USER IDS ONLY, read inside the
// roster transaction so the add path can tell a genuine arrival from a re-send.
//
// IT DECIDES TWO THINGS AT ONCE and both need the same answer: which people are
// news (the `trip_added` fan-out, and the owner's MYR-618 banner) and which
// adds are worth an audit row. A single read is what keeps them consistent —
// two probes could disagree if a concurrent leave landed between them, and the
// row would then record an add that nobody was told about.
const queryTripLiveParticipantUserIDs = `
SELECT user_id FROM go_trip_participants
WHERE trip_id = $1 AND left_at IS NULL`

// queryTripRoleForUser is the CHEAP role probe: the caller's relationship to a
// trip, and the car it is on, without decrypting a name or decorating anything.
//
// It exists for the two MYR-618 routes that need the 404-not-403 rule applied
// BEFORE they do their real work — the addable-people read and the participant
// add — and a NULL role is ErrTripNotFound, identically to an unknown id.
//
// ⚠ IT IS THE ONE STATEMENT THAT USES tripMemberRoleExpr RATHER THAN
// tripRoleExpr, and the difference is invariant 3, not a style choice. Every
// other user of tripRoleExpr is a READ that decides what a card SAYS; these two
// routes are the only ones where the probe's answer is a CAPABILITY — to widen
// a roster, and to enumerate the car's grant-holders by name. `tripRoleExpr`'s
// participant arm tests `left_at IS NULL` alone, so it resolves `participant`
// for somebody whose grant on the car has been SUSPENDED or REVOKED: their map
// has gone dark (the access legs re-join the live grant) while their membership
// row survives, and without the conjunction below they could still add people
// to the trip and read the picker. See tripMemberRoleExpr.
const queryTripRoleForUser = `
SELECT t.vehicle_id, t.owner_user_id, t.starts_at, t.ends_at, t.ended_at,` + tripMemberRoleExpr + `
FROM go_trips t
WHERE t.id = $2`

// queryTripAddablePeople is §7.30.11: the vehicle's live grant-holders who are
// NOT already on this trip.
//
// ⚠ IT CARRIES THE FULL LIVE-GRANT PREDICATE (invariant 3), not because this
// statement grants anything — it is a read — but because its rows are what a
// client posts straight back to §7.30.4's add. A picker that offered a
// suspended grant would produce a `participant_not_shared` refusal the person
// could not explain, having just been shown the name.
//
// NAMES ONLY. The projection is (share id, label, confirmed name) and nothing
// else, and the first two are folded into ONE display string in Go before they
// leave the repository. What §7.5's owner-only grant listing carries and this
// statement never selects: the invite CODE (a credential), the invitee's email,
// `status`, `permission`, `allow_rides`, `suspended_at` and
// `accepted_by_user_id` — the caller may be a PARTICIPANT rather than the
// owner, and §7.5 is owner-only for reasons that have not changed.
//
// ⚠ THE LABEL IS NOT WITHHELD, AND CALLING IT "THE OWNER'S PRIVATE MEMO" WOULD
// BE FALSE (review finding 6). It is the documented FALLBACK half of the
// display name, exactly as it is on the roster: the accepting account's
// confirmed first name wins, and `COALESCE` reaches the label only when there
// is none. The naming prompt (MYR-583) makes that rare, and a blank picker row
// is worse for everybody than the nickname the owner typed. The rule is stated
// where a reader will meet it — §7.30.11's `displayName` row and the handler's
// own comment — rather than implied by an inaccurate claim about what is kept
// back.
//
// The OWNER is excluded because an owner holds no grant on their own car and
// therefore cannot appear here anyway; the first `NOT EXISTS` clause is what
// excludes people already on the trip, and it tests the live roster
// (`left_at IS NULL`) so somebody who LEFT can be re-invited.
//
// ⚠ THE SECOND `NOT EXISTS` IS THE ONE PLACE THIS LIST DIFFERS BY CALLER, and
// it is the review round's owner-removal rule (migration 0061). Somebody the
// OWNER removed is withheld from a PARTICIPANT's picker — offering them would
// be offering a name that §7.30.4 will refuse with `participant_owner_removed`,
// which is the exact "a refusal the person could not explain" this statement's
// live-grant predicate exists to prevent.
//
// **THE OWNER STILL SEES THEM** ($2 = 'owner' switches the clause off), because
// the owner is the only person who may undo their own removal and a picker that
// hid the row would strand it forever. It is a deliberate, narrow departure
// from §7.30.11's "one list, so the two pickers cannot offer different people":
// the pickers now differ by exactly the set of people the owner has removed,
// and the alternative was a rule with no way to reverse it.
//
// ORDERED BY SHARE ID HERE AND BY DISPLAY NAME IN GO, deliberately. The name a
// caller reads is `COALESCE(confirmed first name, label)` and the first half of
// that is an aliased CASE expression — Postgres will order by a bare output
// name but not by an expression over one, so sorting here would order by the
// LABEL and produce a list whose visible order looked arbitrary. The SQL order
// is a total one so the page is deterministic; the reader-facing order is
// applied once the two halves have been folded together.
const queryTripAddablePeople = `
SELECT s.id, COALESCE(s.label, ''), ` + acceptedByNameExpr + `
FROM go_trips t
JOIN go_vehicle_shares s ON s.vehicle_id = t.vehicle_id
WHERE t.id = $1
  AND s.status = 'accepted' AND s.suspended_at IS NULL
  AND s.accepted_by_user_id IS NOT NULL AND s.accepted_by_user_id <> ''
  AND s.accepted_by_user_id <> t.owner_user_id
  AND NOT EXISTS (
        SELECT 1 FROM go_trip_participants p
        WHERE p.trip_id = t.id
          AND p.user_id = s.accepted_by_user_id
          AND p.left_at IS NULL
  )
  AND (
        $2 = 'owner'
        OR NOT EXISTS (
              SELECT 1 FROM go_trip_participants p
              WHERE p.trip_id = t.id
                AND p.user_id = s.accepted_by_user_id
                AND p.removed_by_owner
        )
  )
ORDER BY s.id`

// queryLeaveTrip is the participant's OWN §7.30.6 exit. Idempotent by the
// `left_at IS NULL` guard: a second call updates zero rows and the handler
// still answers 204.
//
// ⚠ IT DELIBERATELY DOES NOT TOUCH `removed_by_owner`. Until the review round
// this statement was described as serving the owner's remove too; it never did
// — that path is queryRemoveTripParticipant — and since migration 0061 the two
// are not interchangeable even in principle. Somebody who walks away has not
// been removed, and stamping the marker here would make a self-leave
// un-reversible by anybody but the owner, which is the opposite of what leaving
// means: a person who left may be invited back by any member.
const queryLeaveTrip = `
UPDATE go_trip_participants
SET left_at = NOW()
WHERE trip_id = $1 AND user_id = $2 AND left_at IS NULL`

// queryLeaveTripByShare is the REVOKED-SHARE CASCADE (see TripRepo.
// RemoveParticipantsForShare). Keyed on (vehicle, user) rather than on the
// share id, so a grant that was revoked and re-issued under a new id still
// removes the person from the trips they joined under the old one.
//
// Scoped to trips that have not ENDED: rewriting the roster of a finished trip
// would rewrite history for no benefit — the window is closed, the access is
// already gone, and the roster is the only record of who was on it.
//
// ⚠ IT DOES NOT STAMP `removed_by_owner` EITHER, and the reason is that this
// ONE statement serves BOTH severing paths — an owner's §7.5.3 revoke and a
// grantee's own §7.5.7 leave — and cannot tell them apart. Nothing turns on the
// choice: a person whose grant on the car is gone is refused by the add's live
// grant predicate long before the marker would be read, so the marker would be
// a claim with no consequence, and on the leave arm it would be a false one.
const queryLeaveTripByShare = `
UPDATE go_trip_participants p
SET left_at = NOW()
FROM go_trips t
WHERE t.id = p.trip_id
  AND t.vehicle_id = $1
  AND p.user_id = $2
  AND p.left_at IS NULL
  AND NOW() < COALESCE(t.ended_at, t.ends_at)`

// tripMemberRoleExpr is tripRoleExpr WITH THE LIVE-GRANT RE-JOIN (invariant 3),
// and it exists for the two MYR-618 capability routes alone.
//
// ── WHY IT IS A SECOND EXPRESSION AND NOT A REPLACEMENT ─────────────────────
//
// The two differ in ONE conjunct and they answer two different questions.
//
//	tripRoleExpr        "is this person ON the roster?"        → DISPLAY
//	tripMemberRoleExpr  "may this person ACT as a member?"     → CAPABILITY
//
// Substituting this one everywhere would be wrong in the direction that hurts:
// a participant whose grant was suspended would stop being able to READ the
// trip they are on — the roster would vanish from their app mid-window — which
// is exactly the "silently dropped mid-drive" failure §7.30's kill-switch note
// forbids. Their live LOCATION is already gone, structurally, through the four
// access legs; the card they were shown is not a capability and does not have
// to disappear with it.
//
// Substituting tripRoleExpr HERE is wrong in the other direction and is a real
// escalation: a revoked or suspended grant-holder could still widen an owner's
// roster (§7.30.4) and enumerate the car's grant-holders by name (§7.30.11)
// long after the owner cut them off. A membership row is not revoked when a
// grant is — the cascade that stamps `left_at` is a display repair by its own
// documentation (trips.md §6) and cannot be relied on for this.
//
// The OWNER arm is untouched: an owner holds no grant on their own car.
//
// The conjunction is the SAME `status = 'accepted' AND suspended_at IS NULL`
// pair every other copy of invariant 3 carries, joined on (vehicle, user) the
// way internal/auth/queries.go's fourth UNION leg joins it, so a grant revoked
// and re-issued under a new id still resolves.
const tripMemberRoleExpr = `
	CASE
		WHEN t.owner_user_id = $1 THEN 'owner'
		WHEN EXISTS (
			SELECT 1 FROM go_trip_participants p
			JOIN go_vehicle_shares s
			  ON s.vehicle_id = t.vehicle_id AND s.accepted_by_user_id = p.user_id
			 AND s.status = 'accepted' AND s.suspended_at IS NULL
			WHERE p.trip_id = t.id AND p.user_id = $1 AND p.left_at IS NULL
		) THEN 'participant'
	END AS trip_role`
