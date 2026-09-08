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
const queryUpsertTripParticipant = `
INSERT INTO go_trip_participants (trip_id, user_id, share_id)
VALUES ($1, $2, $3)
ON CONFLICT (trip_id, user_id) DO UPDATE
SET left_at = NULL, share_id = EXCLUDED.share_id`

// queryLeaveTrip is BOTH the participant's own "leave" and the owner's
// "remove", because they write the same row the same way and the difference is
// only who was allowed to ask. Idempotent by the `left_at IS NULL` guard: a
// second call updates zero rows and the handler still answers 204.
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
const queryLeaveTripByShare = `
UPDATE go_trip_participants p
SET left_at = NOW()
FROM go_trips t
WHERE t.id = p.trip_id
  AND t.vehicle_id = $1
  AND p.user_id = $2
  AND p.left_at IS NULL
  AND NOW() < COALESCE(t.ended_at, t.ends_at)`

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
const queryTripRoster = `
SELECT p.share_id, p.user_id, COALESCE(s.label, ''), ` + acceptedByNameExpr + `
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

// queryTripDriveCount counts the window's drives. See queryTripDrivesWindow for
// why the TEXT column is cast.
const queryTripDriveCount = `
SELECT COUNT(*) FROM "Drive"
WHERE "vehicleId" = $1
  AND "startTime"::timestamptz >= $2
  AND "startTime"::timestamptz <= $3`

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
// #nosec G101 -- an SQL statement naming a token COLUMN, not a credential.
const queryUpsertTripActivityToken = `
INSERT INTO go_trip_activity_tokens (trip_id, user_id, push_to_start_token, sandbox)
VALUES ($1, $2, $3, $4)
ON CONFLICT (trip_id, user_id) DO UPDATE
SET push_to_start_token = EXCLUDED.push_to_start_token,
    sandbox = EXCLUDED.sandbox,
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
const queryTripWindowsForUserVehicle = `
SELECT t.starts_at, LEAST(t.ends_at, COALESCE(t.ended_at, t.ends_at))
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
const queryTripDrivesWindow = `SELECT ` + driveSummarySelectColumns + `
FROM "Drive"
WHERE "vehicleId" = $1
  AND "startTime"::timestamptz >= $2
  AND "startTime"::timestamptz <= $3
ORDER BY "startTime" DESC, "id" DESC
LIMIT $4`

// queryTripDrivesWindowCursor is the resume page. The cursor comparison stays
// TEXT, byte-identical to §7.2's, so the two lists page the same way.
const queryTripDrivesWindowCursor = `SELECT ` + driveSummarySelectColumns + `
FROM "Drive"
WHERE "vehicleId" = $1
  AND "startTime"::timestamptz >= $2
  AND "startTime"::timestamptz <= $3
  AND ("startTime", "id") < ($4, $5)
ORDER BY "startTime" DESC, "id" DESC
LIMIT $6`

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
const queryVehicleDrivesInTripWindows = `SELECT ` + driveSummarySelectColumns + `
FROM "Drive" d
WHERE d."vehicleId" = $1
  AND EXISTS (
	SELECT 1 FROM unnest($2::timestamptz[], $3::timestamptz[]) AS w(win_from, win_to)
	WHERE d."startTime"::timestamptz >= w.win_from
	  AND d."startTime"::timestamptz <= w.win_to
  )
ORDER BY d."startTime" DESC, d."id" DESC
LIMIT $4`

// queryVehicleDrivesInTripWindowsCursor is the resume page. Cursor comparison
// stays TEXT, byte-identical to §7.2's own, so the owner's list and the
// participant's narrowed list page the same way.
const queryVehicleDrivesInTripWindowsCursor = `SELECT ` + driveSummarySelectColumns + `
FROM "Drive" d
WHERE d."vehicleId" = $1
  AND EXISTS (
	SELECT 1 FROM unnest($2::timestamptz[], $3::timestamptz[]) AS w(win_from, win_to)
	WHERE d."startTime"::timestamptz >= w.win_from
	  AND d."startTime"::timestamptz <= w.win_to
  )
  AND (d."startTime", d."id") < ($4, $5)
ORDER BY d."startTime" DESC, d."id" DESC
LIMIT $6`

// queryActiveTripIDForUserVehicle answers VehicleSummary.activeTripId: the id
// of the open window on this car that this caller is party to, as OWNER or as
// live participant.
//
// The owner arm has no share join — an owner holds no grant — and the
// participant arm carries the full live-share predicate, so the field can never
// name a trip whose access the caller does not actually have.
const queryActiveTripIDForUserVehicle = `
SELECT t.id FROM go_trips t
WHERE t.vehicle_id = $2
  AND t.starts_at <= NOW() AND NOW() < COALESCE(t.ended_at, t.ends_at)
  AND (
		t.owner_user_id = $1
		OR EXISTS (
			SELECT 1 FROM go_trip_participants p
			JOIN go_vehicle_shares s
			  ON s.vehicle_id = t.vehicle_id AND s.accepted_by_user_id = p.user_id
			 AND s.status = 'accepted' AND s.suspended_at IS NULL
			WHERE p.trip_id = t.id AND p.user_id = $1 AND p.left_at IS NULL
		)
	)
ORDER BY t.starts_at DESC
LIMIT 1`

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
