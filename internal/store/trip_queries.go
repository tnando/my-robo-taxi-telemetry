package store

// Every SQL statement the MYR-602 trips repository issues, in one file so the
// authorization-relevant statement set can be read at once — the same
// convention internal/auth/queries.go and vehicle_share_queries.go follow, and
// for the same reason: several of these WHERE clauses ARE the access control.
//
// ⚠ A DELIBERATE EXCEPTION TO THE 300-LINE FILE CAP (CLAUDE.md "File Rules"),
// and the only one MYR-602 claims. The cap exists so a file has one subject;
// this file's subject is "the statements, together", and the three invariants
// below are properties OF THE SET rather than of any statement in it. Split by
// operation — reads here, writes there, claims somewhere else — invariant 2
// becomes four spellings of one window predicate in four files, each of them
// locally correct, and the drift it warns about becomes undetectable by
// reading. The neighbours this file names carry the same exemption for the same
// reason: internal/auth/queries.go is 167 lines only because it has fewer
// statements, not because it is organised differently.
//
// The trips code that is NOT a statement was split out rather than left here:
// trip_repo_read.go, trip_repo_write.go, trip_repo_end.go, trip_repo_catalog.go
// and trip_repo_drives.go are each well inside the cap.
//
// ⚠ ONE STATEMENT GROUP LIVES IN A SIBLING FILE: trip_participant_queries.go
// holds everything that reads or writes go_trip_participants — the roster
// upsert, the two departures, the revoked-share cascade, the two MYR-618
// capability statements and tripMemberRoleExpr. It is NOT a split by operation
// (the thing invariant 2 warns about): it is one relation's whole statement
// set, moved together, and the three invariants below hold across BOTH files as
// one. Read them as one file that happens to be stored in two. That file's own
// header carries the pointer back.
//
// THREE INVARIANTS HOLD ACROSS THE FILE.
//
//  1. OWNER-SCOPED MUTATIONS CARRY `owner_user_id = $n` IN THE STATEMENT. The
//     handler's ownership check produces the good error message; this predicate
//     is what actually prevents one person mutating another's trip, on the same
//     row it writes, so there is no check-then-write window.
//
//  2. THE WINDOW PREDICATE IS SPELLED ONE WAY EVERYWHERE:
//     `starts_at <= NOW() AND NOW() < COALESCE(ended_at, ends_at)` — half-open,
//     COALESCE for the early end. It matches store.Trip.StatusAt and
//     internal/auth/queries.go's fourth UNION leg CHARACTER FOR CHARACTER. A
//     surface that called a trip active while the access query called it over
//     would render a live card over a socket that had already dropped the car.
//
//  3. THE SHARE JOIN IS NOT DECORATION. Wherever a participant's ACCESS is at
//     stake the statement re-joins go_vehicle_shares on (vehicle, user) and
//     re-tests `status = 'accepted' AND suspended_at IS NULL`. That is what
//     makes "trip access can never outlive the share" structural rather than a
//     cleanup job — see the inventory of this predicate's copies in
//     vehicle_share_access_queries.go. Statements that serve only DISPLAY (the
//     roster) join the share by id instead, because a name should not vanish
//     from a historical roster the moment a grant is revoked.
//
//     ⚠ THE ROLE EXPRESSION COMES IN TWO SPELLINGS FOR EXACTLY THAT REASON
//     (MYR-618). `tripRoleExpr` answers "is this person on the roster" and
//     feeds the DISPLAY reads; `tripMemberRoleExpr` answers "may this person
//     ACT as a member" and carries the conjunction, and it feeds the two
//     capability routes alone (§7.30.4's add and §7.30.11's picker) through
//     queryTripRoleForUser. Each expression's own comment argues why
//     substituting the other would be wrong, and in which direction.

// tripColumns is the full go_trips projection. Ordered as the struct is, so
// scanTrip reads straight down.
const tripColumns = `
	id, vehicle_id, owner_user_id, name_enc, starts_at, ends_at, ended_at,
	started_notified_at, ended_notified_at, created_at, updated_at`

// queryInsertTrip creates the window. `created_at` / `updated_at` are left to
// their column defaults and returned, the convention every go_ table follows,
// so the row's clock is the DATABASE's and one response cannot report two
// instants for the same write.
const queryInsertTrip = `
INSERT INTO go_trips (id, vehicle_id, owner_user_id, name_enc, starts_at, ends_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at`

// queryTripOverlaps is the 409 probe: does another LIVE window on the same
// vehicle intersect the proposed one?
//
// THE INTERSECTION TEST is the standard half-open one — two intervals overlap
// iff each starts before the other ends — written against the EFFECTIVE end so
// a trip the owner already ended early stops blocking at the instant they
// ended it rather than at its original ends_at.
//
// `NOW() < COALESCE(ended_at, ends_at)` IS THE LOAD-BEARING THIRD PREDICATE and
// it is what makes backfill work. A trip may start in the past — that is the
// stated product requirement, it is how the legs already driven join the trip —
// so a new window will routinely cover instants that OLD, FINISHED windows also
// covered. Only trips that are still scheduled or active can conflict; an ended
// one is history and history does not reserve the calendar.
//
// $4 excludes a trip from its own probe, so PATCH can extend a window without
// colliding with itself. Passed as the empty string on create, which matches no
// id.
const queryTripOverlaps = `
SELECT EXISTS (
	SELECT 1 FROM go_trips
	WHERE vehicle_id = $1
	  AND id <> $4
	  AND NOW() < COALESCE(ended_at, ends_at)
	  AND starts_at < $3
	  AND $2 < COALESCE(ended_at, ends_at)
)`

// tripRoleExpr resolves the caller's relationship to a trip in the SAME
// statement that reads it, so there is no read-then-authorize window and no
// second round trip.
//
// `owner` beats `participant` — an owner who somehow also holds a participant
// row is an owner. NULL means neither, and every read path turns NULL into
// ErrTripNotFound rather than a denial: a trip the caller is not on must be
// indistinguishable from a trip that does not exist, or the endpoint is an
// oracle for trip ids.
//
// A participant who LEFT resolves NULL. Leaving is meant to end the
// relationship, not to leave a read-only souvenir.
//
// ⚠ IT DOES NOT RE-JOIN THE LIVE GRANT, and that is correct HERE and wrong for
// the two MYR-618 capability routes — see tripMemberRoleExpr, which is this
// expression plus invariant 3's conjunction, and which is what
// queryTripRoleForUser uses.
const tripRoleExpr = `
	CASE
		WHEN t.owner_user_id = $1 THEN 'owner'
		WHEN EXISTS (
			SELECT 1 FROM go_trip_participants p
			WHERE p.trip_id = t.id AND p.user_id = $1 AND p.left_at IS NULL
		) THEN 'participant'
	END AS trip_role`

// queryTripByIDForUser reads one trip together with the caller's role.
const queryTripByIDForUser = `
SELECT ` + tripColumns + `,` + tripRoleExpr + `
FROM go_trips t
WHERE t.id = $2`

// queryTripsForUser is GET /api/trips: every trip the caller owns or is a live
// participant of, newest first, optionally filtered by derived status.
//
// THE STATUS FILTER IS COMPUTED IN SQL rather than read back and filtered in Go,
// because `limit` has to mean "N trips of the requested status" — filtering
// after the LIMIT would return a short page (or an empty one) while more
// matching trips sat behind it. The three arms restate the window predicate a
// third time; they agree with StatusAt by inspection and by
// TestTripStatusFilterAgreesWithStatusAt.
//
// $2 = ” means "no filter", which is why the enum values can be compared
// directly without a nullable parameter.
//
// ORDER BY created_at DESC, id DESC — the same total order every other list in
// this codebase uses, so ties are stable across pages.
const queryTripsForUser = `
SELECT ` + tripColumns + `,` + tripRoleExpr + `
FROM go_trips t
WHERE (
		t.owner_user_id = $1
		OR EXISTS (
			SELECT 1 FROM go_trip_participants p
			WHERE p.trip_id = t.id AND p.user_id = $1 AND p.left_at IS NULL
		)
	)
  AND (
		$2 = ''
		OR ($2 = 'scheduled' AND NOW() < t.starts_at)
		OR ($2 = 'active' AND t.starts_at <= NOW() AND NOW() < COALESCE(t.ended_at, t.ends_at))
		OR ($2 = 'ended' AND NOW() >= COALESCE(t.ended_at, t.ends_at))
	)
ORDER BY t.created_at DESC, t.id DESC
LIMIT $3`

// queryUpdateTripWindow rewrites the mutable fields. Both are passed on every
// call with the CURRENT value standing in for "unchanged", so PATCH has one
// statement rather than a dynamically assembled one — the shape MYR-369's
// share patch settled on for the same reason.
//
// `updated_at = NOW()` unconditionally, including on a no-op patch: the column
// answers "when did somebody last touch this row", and a writer that skipped it
// for an unchanged value would make the answer depend on what changed.
const queryUpdateTripWindow = `
UPDATE go_trips
SET name_enc = $3, ends_at = $4, updated_at = NOW()
WHERE id = $1 AND owner_user_id = $2
RETURNING ` + tripColumns

// queryEndTrip is the owner's early end. IDEMPOTENT BY `ended_at IS NULL`: a
// second call updates zero rows, the caller re-reads, and the endpoint answers
// 200 with the already-ended trip. Re-stamping would move the end forward on
// every retry, which for an ACCESS boundary means a double-tap silently
// extending somebody's live location by however long the two taps were apart.
const queryEndTrip = `
UPDATE go_trips
SET ended_at = NOW(), updated_at = NOW()
WHERE id = $1 AND owner_user_id = $2 AND ended_at IS NULL`

// queryTripRoster is the participant list for one trip, for DISPLAY.
//
// It joins the share BY ID, not by (vehicle, user) with the live-grant
// predicates, and that is the one deliberate departure from invariant 3 at the
// top of this file. This statement decides no access — the access query in
// internal/auth does, on every resolution — it decides what a name says. A
// roster that dropped a person the instant their grant was suspended would make
// the trip card silently disagree with itself mid-window.
//
// The NAME follows the roster rule (MYR-581/583): the accepting account's
// CONFIRMED first name if there is one, else the owner's own label for the
// grant. `acceptedByNameExpr` carries the confirmation gate and the three-rung
// ladder; ownerFirstNameToken reduces it to a first token in Go.
// ⚠ LEFT JOIN, NOT INNER, and the direction of that choice is the point. This
// roster is what an OWNER reads to know who can see their car. An inner join
// would DROP a participant whose share row had somehow gone — under-reporting
// on a privacy surface, silently, in the one direction that matters. Nothing in
// production deletes a go_vehicle_shares row today (revocation is a tombstone
// flip, and even account deletion revokes rather than deletes), so this arm is
// unreachable; it is written this way so that if that ever changes, the failure
// is a nameless row rather than a missing person.
//
// `accepted_by_user_id` is the column acceptedByNameExpr keys on, so a NULL
// share row yields a NULL name and the Go side falls back — first to the
// owner's label, which is also NULL here, and then to the empty string the
// contract permits.
//
// `added_by_name` (MYR-618) is the CONFIRMED first name of whoever put this
// person on the trip, resolved through the ladder in trip_added_by_name.go and
// NULL for every row written before migration 0060. It is projected here rather
// than joined in a second read for the same reason the label is: the roster is
// read on every trip card, and an attribution that needed a second query would
// be the first thing a future optimisation dropped.
//
// ── ⚠ EIGHT CORRELATED SUBSELECTS PER ROW, AND THAT IS KNOWN ────────────────
//
// `acceptedByNameExpr` and `addedByNameExpr` are each a confirmation EXISTS
// plus a three-rung COALESCE of scalar subselects — four apiece — so a roster
// of N people issues 8N correlated lookups against `"User"`,
// `go_identity_apple`, `go_users` and `go_profile_name_confirmations`. Every
// one of them is a primary-key or unique-index probe, and a roster is a handful
// of people, so a single trip read is cheap in practice.
//
// **THE COST THAT IS REAL IS ON `GET /api/trips` (§7.30.2), and it is
// PRE-EXISTING rather than something MYR-618 introduced.** That list decorates
// every trip it returns, so a page of T trips runs the roster read T times and
// the subselect count is 8 × (total participants). MYR-618 doubled the per-row
// constant by adding the second ladder; it did not change the shape.
//
// **DELIBERATELY NOT FIXED HERE.** The fix is a batched name resolution —
// collect every user id across every roster in the response and resolve them in
// ONE pass — which is a change to the read path's structure, touching the
// catalog's owner name and the ride surfaces by the same argument. Doing it
// inside a roster-widening change would mean rewriting the statement that
// decides what a name says in a PR whose subject is who may add people.
// Recorded so the next person to profile §7.30.2 finds the reasoning rather
// than rediscovering it.
const queryTripRoster = `
SELECT p.share_id, p.user_id, COALESCE(s.label, ''), ` + acceptedByNameExpr + `, ` + addedByNameExpr + `
FROM go_trip_participants p
LEFT JOIN go_vehicle_shares s ON s.id = p.share_id
WHERE p.trip_id = $1 AND p.left_at IS NULL
ORDER BY p.added_at, p.user_id`

// queryTripOwnerFirstName is the confirmed-only name ladder keyed on
// go_trips.owner_user_id.
//
// A SIBLING of ownerNameLadderExpr rather than a reuse of it, for the reason
// profile_name_confirmation.go states about its own duplicate: the embedding
// statements are `const`, so the key column cannot be a parameter. Keying it on
// the TRIP's owner rather than on "Vehicle"."userId" is not pedantry — a car
// can change hands, and the trip's card names the person whose trip it is.
const queryTripOwnerFirstName = `
SELECT CASE WHEN EXISTS (
		SELECT 1 FROM go_profile_name_confirmations pnc WHERE pnc.user_id = t.owner_user_id
	) THEN COALESCE(
		NULLIF(TRIM((SELECT u."name" FROM "User" u WHERE u."id" = t.owner_user_id)), ''),
		NULLIF(TRIM((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = t.owner_user_id ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."name" FROM go_users g WHERE g.id = t.owner_user_id)), '')
	) END
FROM go_trips t WHERE t.id = $1`

// queryTripVehicle reads the catalog subset the trip card renders, so a
// participant's card needs no second call. READ-ONLY against the Prisma-owned
// relation, which CG-DL-9 permits.
// THE TRIM PAIR RIDES THE go_vehicle_control_state JOIN, not the "Vehicle" row,
// and that is not an implementation detail to look up twice: `trim_label` (the
// display-safe label Tesla returns) and `trim` (the raw badge code) are both
// Go-owned columns on the side table, exactly where the catalog and the
// snapshot read them from. Selecting them off "Vehicle" is a column that does
// not exist, which is how this statement failed the first time it ran.
//
// LEFT JOIN, because a car with no control-state row yet is a real state — a
// vehicle linked seconds ago — and a trip card for it must render with a null
// trim rather than not render at all.
const queryTripVehicle = `
SELECT v."id", v."name", v."model", v."year", v."color", v."vin",
       gcs.trim_label, gcs.trim
FROM "Vehicle" v
JOIN go_trips t ON t.vehicle_id = v."id"
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = v."id"
WHERE t.id = $1`

// queryTripDriveTotals is the ONE STATEMENT behind `driveCount`,
// `totalDistanceMiles` and `totalDurationSeconds` (MYR-608). See
// queryTripDrivesWindow for why the TEXT column is cast.
//
// THE THREE NUMBERS COME BACK TOGETHER, and that is the whole N+1 argument.
// §7.30.2 decorates every row it returns, so a separate SUM query would have
// added one round trip PER TRIP to a list that already issues five. Widening
// the count that was already there adds none: the totals ride the scan the
// count was paying for.
//
// SUM OVER ZERO ROWS IS NULL, and the nulls are carried to the wire rather
// than coalesced to zero. `null` means "this window holds no drives"; `0`
// means "it holds drives that summed to nothing" — a legitimate reading for a
// window of stationary or discarded micro-drives — and collapsing the two
// would tell a client a trip covered zero miles when the truth is that it has
// nothing to report yet.
//
// RUNNING TOTALS, NOT FINAL ONES. They are computed at read time like
// everything else about a window-scoped feature, so an ACTIVE trip reports
// what it has driven SO FAR and the number climbs between reads. Withholding
// them until the window closed would leave the surface that most wants a
// total — a road trip in progress — the one surface that cannot have one.
const queryTripDriveTotals = `
SELECT COUNT(*), SUM("distanceMiles"), SUM("durationMinutes") FROM "Drive"
WHERE "vehicleId" = $1
  AND "startTime"::timestamptz >= $2
  AND "startTime"::timestamptz <= $3`

// ── THE THREE tripId EXPRESSIONS (MYR-608) ──────────────────────────────────
//
// `DriveSummary.tripId` answers, for one drive row, "which trip's window does
// this drive belong to, as far as THIS caller is concerned". It is a JOIN
// evaluated at read time, never a column: nothing tags a drive at write time,
// which is what lets a back-dated window sweep up legs already driven.
//
// THEY ARE THREE BECAUSE THE ROLE IS THREE DIFFERENT QUESTIONS, and collapsing
// them into one would be the access bug this file's invariant 3 exists to
// prevent. An OWNER's answer ranges over the windows they own. A PARTICIPANT's
// ranges over the windows that admitted them — the live-share join included —
// and never over the owner's other trips. §7.30.7's answer is the trip in the
// PATH, which the caller has already been authorized on.
//
// WHAT THEY SHARE IS THE WINDOW PREDICATE, and it is factored so the sharing
// is literal rather than by inspection: `tripEffectiveEndExpr` is the ONE
// spelling of `min(endsAt, endedAt)` in this file, and every bound below is
// INCLUSIVE at both ends — the same bound §7.30.7, the drive count and the
// §7.2/§7.3/§7.4 admission use, so all five agree about which drives are in a
// window.

// tripEffectiveEndExpr is `min(endsAt, endedAt)` over an aliased `go_trips t`.
//
// ONE SPELLING, referenced by every statement that needs it, because this
// expression is the trips model's second load-bearing rule (invariant 2 is the
// first) and a second copy is how a surface starts disagreeing with the
// predicate that decides access. COALESCE first so a trip that was never ended
// early yields its scheduled end rather than NULL.
const tripEffectiveEndExpr = `LEAST(t.ends_at, COALESCE(t.ended_at, t.ends_at))`

// tripCoversDriveExpr is the drive-membership test: does the drive row aliased
// `d` begin inside trip `t`'s window?
//
// THE CAST IS NOT OPTIONAL — see queryTripDrivesWindow. `Drive."startTime"` is
// a Prisma-owned TEXT column, and comparing it as text would make this
// expression agree with the list it decorates only while every row carries the
// same offset spelling.
const tripCoversDriveExpr = `t.starts_at <= d."startTime"::timestamptz
		  AND d."startTime"::timestamptz <= ` + tripEffectiveEndExpr

// ownerTripIDForDriveExpr resolves the tripId on the OWNER's §7.2 list.
//
// ⚠ THE CALLER IS HARD-CODED AT $2, and both `queryDriveListByVehicle` and
// `queryDriveListByVehicleCursor` bind them there. A function taking a
// placeholder name would have made those two statements assembled strings, and
// this file's whole posture is that a predicate near an access decision is a
// `const` a reader can check — so the placeholder is fixed and the two call
// sites are the thing that must agree, which the compiler-free part of that is
// pinned by TestDriveListCarriesTheOwnersTripID.
//
// SCOPED TO `owner_user_id = $2` RATHER THAN TO THE VEHICLE. A car can change
// hands, and a drive that fell in the PREVIOUS owner's window is not this
// caller's trip; naming it would disclose that a stranger's trip existed and
// hand its id to somebody who cannot read it. §7.2 is owner-only, so this is
// the only role that reaches this statement.
//
// NEWEST WINS on an overlap. Two LIVE windows on one car are refused by the
// create probe, but two ENDED ones may overlap freely (the probe only guards
// windows that have not closed), so a drive can sit in two of them. One row
// carries one id, so the tie is broken deterministically — latest `starts_at`,
// then the id — rather than left to the planner.
const ownerTripIDForDriveExpr = `
	(SELECT t.id FROM go_trips t
	 WHERE t.vehicle_id = d."vehicleId"
	   AND t.owner_user_id = $2
	   AND ` + tripCoversDriveExpr + `
	 ORDER BY t.starts_at DESC, t.id DESC
	 LIMIT 1) AS trip_id`

// participantTripIDForDriveExpr resolves the tripId on the PARTICIPANT's §7.2
// list — the narrowed form in queryVehicleDrivesInTripWindows below.
//
// ⚠ IT RE-USES THE VERY WINDOWS THAT ADMITTED THE ROW rather than re-deriving
// them from the caller. The enclosing statement already unnests the window set
// into `w(win_from, win_to, trip_id)` and gates every row on it; this reads the
// trip id out of the SAME derived table. That is not an optimisation, it is the
// property: the id a participant is told cannot name a trip whose window did
// not admit the drive, because there is only one window set in the statement.
//
// The third array is the reason `TripDrivesWindow` carries a `TripID`.
const participantTripIDForDriveExpr = `
	(SELECT w.trip_id
	 FROM unnest($2::timestamptz[], $3::timestamptz[], $4::text[]) AS w(win_from, win_to, trip_id)
	 WHERE d."startTime"::timestamptz >= w.win_from
	   AND d."startTime"::timestamptz <= w.win_to
	 ORDER BY w.win_from DESC, w.trip_id DESC
	 LIMIT 1) AS trip_id`

// pathTripIDForDriveExpr is §7.30.7's answer: THE TRIP IN THE URL, bound as a
// parameter rather than resolved.
//
// ⚠ A DELIBERATE DIVERGENCE FROM THE TWO EXPRESSIONS ABOVE, and the one place
// on the platform where two surfaces may report a different `tripId` for the
// same drive. It happens only where two ENDED windows overlap, and the reason
// is that the two surfaces are answering different questions. §7.2 asks "which
// trip does this drive belong to" over a whole history and has to pick one;
// this endpoint asks "what are THIS trip's drives", every row it returns is
// inside this trip's window BY THE STATEMENT'S OWN PREDICATE, and a row that
// came back naming a different overlapping trip would make the page disagree
// with the route that produced it — a client grouping by `tripId` would draw a
// section for a trip it did not ask about.
//
// The stamp can never be a lie: the caller was authorized on this trip before
// the statement ran, and the window bound is the same one the WHERE clause
// applies.
const pathTripIDForDriveExpr = `$4::text AS trip_id`

// queryTripOpenLeg reads the leg underway, if any, together with the vehicle's
// CURRENT eta so the card's "arrives in N min" is read at REST-read time rather
// than frozen at leg start.
//
// The leg table is written by internal/trips' leg detector; this repository only
// reads it. `etaMinutes` comes off the vehicle row because that is where live
// navigation lands — the leg row records what the car SAID it was driving to,
// not how far away it is now.
const queryTripOpenLeg = `
SELECT l.started_at, l.destination_name_enc, v."etaMinutes"
FROM go_trip_legs l
JOIN go_trips t ON t.id = l.trip_id
JOIN "Vehicle" v ON v."id" = t.vehicle_id
WHERE l.trip_id = $1 AND l.ended_at IS NULL
ORDER BY l.started_at DESC
LIMIT 1`

// queryUpsertTripActivityToken stores one party's ActivityKit PUSH-TO-START
// token. UPSERT because ActivityKit rotates the value: a re-registration
// REPLACES it rather than accumulating rows that would each try to start their
// own Activity.
//
// P1 CAPABILITY. The value is never logged beyond an 8-character prefix, never
// echoed into a response, never placed in an error message.
//
// ⚠ A ROTATED TOKEN RESETS `started_leg_id` (MYR-612), and an unchanged one
// does not. ActivityKit rotates the value, and the new token addresses a phone
// that holds no card for the current leg — so it must be able to be sent one.
// An idempotent re-POST of the SAME token, which is what an app does on every
// foreground, must NOT: that phone already has its card, and clearing the stamp
// would raise a second one on the same lock screen for the same journey.
//
// #nosec G101 -- an SQL statement naming a token COLUMN, not a credential.
const queryUpsertTripActivityToken = `
INSERT INTO go_trip_activity_tokens (trip_id, user_id, push_to_start_token, sandbox)
VALUES ($1, $2, $3, $4)
ON CONFLICT (trip_id, user_id) DO UPDATE
SET push_to_start_token = EXCLUDED.push_to_start_token,
    sandbox = EXCLUDED.sandbox,
    started_leg_id = CASE
        WHEN go_trip_activity_tokens.push_to_start_token IS DISTINCT FROM EXCLUDED.push_to_start_token
        THEN NULL
        ELSE go_trip_activity_tokens.started_leg_id
    END,
    updated_at = NOW()`

// queryDeleteTripActivityToken is the DELETE half. Idempotent: no row is the
// same answer as one row removed, and the endpoint answers 204 either way.
//
// #nosec G101 -- an SQL statement naming a token COLUMN, not a credential.
const queryDeleteTripActivityToken = `
DELETE FROM go_trip_activity_tokens WHERE trip_id = $1 AND user_id = $2`

// queryTripWindowsForUserVehicle returns every window on ONE vehicle that
// admits the caller to that vehicle's DRIVES — the §7.2/§7.3/§7.4 gate.
//
// ACTIVE **OR ENDED**, which is wider than the live-access predicate and
// deliberately so. Live location is a window-scoped grant that ends with the
// window; the window's DRIVES are the record of a journey the person was part
// of, and a road trip's drive list becoming unreadable the moment the trip ends
// would delete the feature at the exact moment it becomes worth reading.
// SCHEDULED trips are excluded — a window that has not opened contains no
// drives, and admitting one would let an owner grant read access to the past by
// scheduling a trip for next week.
//
// The live-share join still applies: a person whose grant was revoked loses the
// drives with everything else.
//
// #nosec G101 -- an SQL statement naming a token COLUMN, not a credential.
//
// IT RETURNS THE TRIP ID as well (MYR-608). The id is not an access fact — the
// three predicates below are — but the window and the trip it came from must
// travel together: `participantTripIDForDriveExpr` reads the id out of the very
// window set that admitted a row, and a window carried without its id would
// have forced a second resolution, which is a second chance to answer with a
// trip that did not admit the drive.
const queryTripWindowsForUserVehicle = `
SELECT t.starts_at, ` + tripEffectiveEndExpr + `, t.id
FROM go_trip_participants p
JOIN go_trips t ON t.id = p.trip_id
JOIN go_vehicle_shares s
  ON s.vehicle_id = t.vehicle_id AND s.accepted_by_user_id = p.user_id
 AND s.status = 'accepted' AND s.suspended_at IS NULL
WHERE p.user_id = $1 AND t.vehicle_id = $2 AND p.left_at IS NULL
  AND t.starts_at <= NOW()`

// queryTripDrivesWindow is the §7.30.7 drive list: the window's drives, newest
// first, with the §7.2 keyset cursor.
//
// THE CAST IS DELIBERATE. `Drive."startTime"` is a TEXT column holding RFC 3339
// (a Prisma-owned shape this repo may not change), and text ordering only
// matches chronological ordering while every row carries the same offset
// spelling. §7.2's cursor comparison already relies on that and a wrong answer
// there is a pagination glitch; here a wrong answer is an ACCESS decision — a
// participant reading a drive from outside their window — so the bound is
// compared as an instant, not as a string. The cast costs the index on
// startTime for the FILTER; the `vehicleId` index still selects the rows and
// the ordering index still orders them, and one vehicle's drive history is a
// bounded set.
//
// The upper bound is INCLUSIVE where the access predicate's is exclusive: a
// drive that began exactly at the closing instant is a drive of this trip, and
// excluding it would lose it from the only list it belongs to.
//
// $4 IS THE TRIP ID, stamped onto every row — see pathTripIDForDriveExpr for
// why this surface stamps rather than resolves.
const queryTripDrivesWindow = `SELECT ` + driveSummarySelectColumns + `, ` + pathTripIDForDriveExpr + `
FROM "Drive" d
WHERE "vehicleId" = $1
  AND "startTime"::timestamptz >= $2
  AND "startTime"::timestamptz <= $3
ORDER BY "startTime" DESC, "id" DESC
LIMIT $5`

// queryTripDrivesWindowCursor is the resume page. The cursor comparison stays
// TEXT, byte-identical to §7.2's, so the two lists page the same way.
const queryTripDrivesWindowCursor = `SELECT ` + driveSummarySelectColumns + `, ` + pathTripIDForDriveExpr + `
FROM "Drive" d
WHERE "vehicleId" = $1
  AND "startTime"::timestamptz >= $2
  AND "startTime"::timestamptz <= $3
  AND ("startTime", "id") < ($5, $6)
ORDER BY "startTime" DESC, "id" DESC
LIMIT $7`

// queryVehicleDrivesInTripWindows is the §7.2 list AS SEEN BY A TRIP
// PARTICIPANT: the vehicle's drives, narrowed to the union of the windows that
// admit this caller.
//
// THE WINDOWS ARRIVE AS TWO PARALLEL ARRAYS and are unnested into a derived
// table, rather than being assembled into an OR-chain of literals in Go. Two
// reasons, and the second is the binding one: the statement stays a `const`
// with a fixed parameter count, so it cannot be built wrong for a caller with
// three windows; and there is no string concatenation anywhere near an
// access-control predicate.
//
// A CALLER WITH NO WINDOWS MATCHES NOTHING, which is the correct denial and is
// structural rather than checked: unnest over two empty arrays yields no rows,
// so the EXISTS is false for every drive. The handler still refuses before
// reaching here, but a gate that also fails safe one layer down is a gate that
// survives a refactor of the layer above.
//
// The cast on "startTime" is there for the reason queryTripDrivesWindow states.
//
// A THIRD PARALLEL ARRAY carries the trip ids (MYR-608), so the projection can
// name the window that admitted each row without a second resolution — see
// participantTripIDForDriveExpr. The EXISTS still gates on the from/to pair
// alone: the id decorates, it never admits.
const queryVehicleDrivesInTripWindows = `SELECT ` + driveSummarySelectColumns + `, ` + participantTripIDForDriveExpr + `
FROM "Drive" d
WHERE d."vehicleId" = $1
  AND EXISTS (
	SELECT 1 FROM unnest($2::timestamptz[], $3::timestamptz[]) AS w(win_from, win_to)
	WHERE d."startTime"::timestamptz >= w.win_from
	  AND d."startTime"::timestamptz <= w.win_to
  )
ORDER BY d."startTime" DESC, d."id" DESC
LIMIT $5`

// queryVehicleDrivesInTripWindowsCursor is the resume page. Cursor comparison
// stays TEXT, byte-identical to §7.2's own, so the owner's list and the
// participant's narrowed list page the same way.
const queryVehicleDrivesInTripWindowsCursor = `SELECT ` + driveSummarySelectColumns + `, ` + participantTripIDForDriveExpr + `
FROM "Drive" d
WHERE d."vehicleId" = $1
  AND EXISTS (
	SELECT 1 FROM unnest($2::timestamptz[], $3::timestamptz[]) AS w(win_from, win_to)
	WHERE d."startTime"::timestamptz >= w.win_from
	  AND d."startTime"::timestamptz <= w.win_to
  )
  AND (d."startTime", d."id") < ($5, $6)
ORDER BY d."startTime" DESC, d."id" DESC
LIMIT $7`

// queryActiveTripIDsForUser answers "which of the cars in this caller's catalog
// — however they got there — have a window open right now?".
//
// ⚠ IT IS NOT THE SAME QUESTION AS THE MERGE LEG BELOW, AND CONFLATING THEM WAS
// MYR-612. That statement lists the cars a trip ADDS to the catalog, which is
// participant rows only — an owner's cars are the catalog's first leg already.
// This one ANNOTATES rows that are already there, and an owner's own car is the
// row a trip is most often opened on. One statement served both, so `VehicleSummary.activeTripId`
// was never present on an owner's own row: the app registers its ActivityKit
// push-to-start token from `activeTripIDsForActivityTokens`, which reads the
// catalog, so the owner of the car on the trip never registered a token and
// never got a leg card. Nabil, 2026-09-08.
//
// The owner arm carries no share join — an owner holds no grant — and the
// participant arm carries the full live-share predicate, so the field can never
// name a trip whose access the caller does not actually have. It is the same
// two-armed shape the per-vehicle form had, widened from one vehicle
// to all of them, and the window predicate is spelled the one way it is spelled
// everywhere (trips.md §2).
//
// DISTINCT ON (vehicle_id) with the newest window first: a car cannot legally
// carry two open windows — the create endpoint's overlap probe refuses it — but
// the catalog must project ONE id per row whatever the table holds.
const queryActiveTripIDsForUser = `
SELECT DISTINCT ON (vehicle_id) vehicle_id, id FROM (
	SELECT t.vehicle_id, t.id, t.starts_at
	FROM go_trips t
	WHERE t.owner_user_id = $1
	  AND t.starts_at <= NOW() AND NOW() < COALESCE(t.ended_at, t.ends_at)
	UNION ALL
	SELECT t.vehicle_id, t.id, t.starts_at
	FROM go_trip_participants p
	JOIN go_trips t ON t.id = p.trip_id
	JOIN go_vehicle_shares s
	  ON s.vehicle_id = t.vehicle_id AND s.accepted_by_user_id = p.user_id
	 AND s.status = 'accepted' AND s.suspended_at IS NULL
	WHERE p.user_id = $1 AND p.left_at IS NULL
	  AND t.starts_at <= NOW() AND NOW() < COALESCE(t.ended_at, t.ends_at)
) w
ORDER BY vehicle_id, starts_at DESC`

// queryActiveTripVehiclesForUser is the CATALOG's third merge leg: the vehicles
// of the caller's open windows, as catalog rows, with the trip id attached.
//
// PARTICIPANT ROWS ONLY. An owner's cars are already the first leg of that
// response and re-emitting them here would produce a duplicate the dedupe would
// discard anyway; the owner's own activeTripId is resolved per row instead.
const queryActiveTripVehicleIDsForUser = `
SELECT DISTINCT t.vehicle_id, t.id
FROM go_trip_participants p
JOIN go_trips t ON t.id = p.trip_id
JOIN go_vehicle_shares s
  ON s.vehicle_id = t.vehicle_id AND s.accepted_by_user_id = p.user_id
 AND s.status = 'accepted' AND s.suspended_at IS NULL
WHERE p.user_id = $1 AND p.left_at IS NULL
  AND t.starts_at <= NOW() AND NOW() < COALESCE(t.ended_at, t.ends_at)`

// ── ACCOUNT DELETION (step 8g) ──────────────────────────────────────────────
//
// Two statements, because a person stands in two relations to trips and the
// deletions are not the same shape. Ordering between them does not matter; both
// run inside the deletion sequence.

// queryDeleteTripsOwnedBy removes the windows this person CREATED. The three
// child tables cascade off go_trips.id (migration 0047 declares the FKs), so
// participants, push-to-start tokens and legs go with them — which is why this
// is one statement and not four.
const queryDeleteTripsOwnedBy = `DELETE FROM go_trips WHERE owner_user_id = $1`

// queryDeleteTripParticipationsBy removes this person from OTHER people's
// trips. Their memberships are theirs to take with them; the trips are not.
const queryDeleteTripParticipationsBy = `DELETE FROM go_trip_participants WHERE user_id = $1`

// queryDeleteTripActivityTokensBy removes this person's push-to-start tokens on
// other people's trips. The ones on their OWN trips already went with the
// cascade above, and deleting them twice is harmless — the second statement
// finds nothing, which is exactly the idempotency every other deletion step
// has.
//
// #nosec G101 -- an SQL statement naming a token COLUMN, not a credential.
const queryDeleteTripActivityTokensBy = `DELETE FROM go_trip_activity_tokens WHERE user_id = $1`

// ── OWNER DELETION (MYR-607, §7.30.10) ──────────────────────────────────────
//
// Five statements, in the order TripRepo.Delete issues them. They are here
// rather than in trip_repo_delete.go for the reason at the top of this file:
// `queryLockTripForOwner` is an ACCESS-CONTROL predicate, and the set of those
// is meant to be readable at once.

// queryLockTripForOwner is the deletion's ownership gate AND its serialiser.
//
// `owner_user_id = $2` is invariant 1 applied to the most destructive mutation
// on the surface: no row means an unknown trip, somebody else's trip, or a trip
// the caller is only a PARTICIPANT of, and all three are one answer (404).
//
// FOR UPDATE holds the row for the length of the transaction, so two concurrent
// deletes of one trip serialise: the second waits, finds the row gone, and its
// caller receives the same ErrTripNotFound a stranger would. It also blocks a
// concurrent PATCH or END from mutating a trip whose children are being
// removed — both of those write go_trips through statements that take their own
// row lock.
//
// It returns the VEHICLE because the audit row's metadata carries it and
// nothing else: after the commit, the car is the only other nameable thing
// about a window that no longer exists.
const queryLockTripForOwner = `
SELECT vehicle_id FROM go_trips
WHERE id = $1 AND owner_user_id = $2
FOR UPDATE`

// queryDeleteTripLegActivitiesForTrip removes the LEG-ANCHORED Live Activity
// registrations of every leg this trip ran.
//
// ⚠ THE ANCHOR PREDICATE IS THE SCOPE, not a filter over it: the sub-select
// names this trip's legs, and `go_live_activities` rows with a RIDE anchor are
// unreachable from it by construction (the CHECK on migration 0047 permits
// exactly one anchor per row). A statement that reached a ride's registration
// would take the sender's only address for a card that is still on a lock
// screen — the hazard account_deletion_trip_live.go spells out at length.
//
// The FK from `trip_leg_id` would cascade this when the legs go, one statement
// down. It is stated anyway: a two-link cascade is invisible at the call site,
// and the row it silently missed would be a live capability addressed at a
// phone.
const queryDeleteTripLegActivitiesForTrip = `
DELETE FROM go_live_activities
WHERE trip_leg_id IN (SELECT id FROM go_trip_legs WHERE trip_id = $1)`

// queryDeleteTripLegs removes the trip's legs.
const queryDeleteTripLegs = `DELETE FROM go_trip_legs WHERE trip_id = $1`

// queryDeleteTripActivityTokensForTrip removes every party's push-to-start
// registration on this trip — the owner's and each participant's alike.
//
// #nosec G101 -- an SQL statement naming a token COLUMN, not a credential.
const queryDeleteTripActivityTokensForTrip = `
DELETE FROM go_trip_activity_tokens WHERE trip_id = $1`

// queryDeleteTripParticipantsForTrip removes the roster.
//
// A HARD DELETE, unlike queryLeaveTrip's tombstone, and the asymmetry is the
// point: `left_at` exists so "was this person ever on this trip" stays
// answerable, and after the trip itself is gone there is no trip for that
// question to be about.
const queryDeleteTripParticipantsForTrip = `
DELETE FROM go_trip_participants WHERE trip_id = $1`

// queryDeleteTrip removes the window itself. Owner-scoped a second time — the
// FOR UPDATE probe already established it — because invariant 1 is about the
// statement that WRITES, and a predicate that is merely redundant today is what
// keeps it true after the next edit.
const queryDeleteTrip = `DELETE FROM go_trips WHERE id = $1 AND owner_user_id = $2`

// tripDeleteChildStatements is the child-first order TripRepo.Delete issues.
// Ordered rather than a set: the Live Activity rows hang off the legs, so they
// go first, and a reordering that broke that would be caught by the FK rather
// than silently.
var tripDeleteChildStatements = []string{
	queryDeleteTripLegActivitiesForTrip,
	queryDeleteTripLegs,
	queryDeleteTripActivityTokensForTrip,
	queryDeleteTripParticipantsForTrip,
}
