# Trips — window-scoped live sharing (MYR-602)

**Status:** Implemented — migrations **0047** and **0048**, `store.TripRepo`, `telemetry.TripHandler`, the fourth access leg in `internal/auth`, the `viewer` narrowing in `internal/mask`, and `internal/trips` (the window sweeper and the leg detector) with the `trips` push category and the per-leg Live Activity in `internal/push`. Contracts **v0.41.0** ([`schemas/trip.schema.json`](../contracts/schemas/trip.schema.json)). The wire contract is [`rest-api.md`](../contracts/rest-api.md) §7.30, §7.21.7 and §5; the classification is [`data-classification.md`](../contracts/data-classification.md) §1.25–§1.28. Built as two lanes against one base branch — the model, the REST surface and the access resolution (§1–§9), and the legs, the pushes and the re-mask (§10–§11) — joined at the seams in §12.

**Goal.** Give an owner a way to say *"for these three days, these people may see my car"* — and, in the same change, take standing live location away from a share that is not saying that. The feature is one sentence: **a trip is a TIME WINDOW during which the share-holders the owner picked see the car's live location, its navigation, the window's drives and a per-leg Live Activity.**

**Client decisions (Thomas).**

- **2026-09-05, the narrowing:** *"you should really only see live location during an active trip shared with a user"* (or an accepted active ride). This **supersedes** [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435)'s 2026-08-02 decision to keep the navigation group on a plain `viewer` — see §4.
- **2026-09-05, access is the window:** a participant's access is **not** conditioned on a driving leg being underway, on the car's status, or on a destination being set. A parked car inside an open window streams to its participants exactly as it does to its owner. Legs govern the Live Activity and nothing else.
- **2026-09-06, the owner is in the Activity:** the owner may register a push-to-start token and is included in the per-leg Live Activity, even though the owner receives none of the three REST-caused pushes.
- **2026-09-07, the trip is deletable:** *"please allow the creator of a trip to go back and add more people, and have the ability delete the trip at anytime"* ([MYR-607](https://linear.app/myrobotaxi/issue/MYR-607)). Adding people later was already `PATCH`'s `addParticipantIds`; deleting was not, and **"at anytime" is taken literally — every status, `ended` included.** See §10A.

---

## 1. What a trip is

**A trip is a `(vehicle, window, participant set)` tuple owned by the vehicle's owner, and it creates NO new vehicle relationship.**

Participants are picked from the car's **already-accepted `go_vehicle_shares` grants** — that is literally where the picker's candidate rows come from, and `CreateTripInput.ParticipantShareIDs` carries **share ids**, not user ids. The trip decides only **what that existing grant means between two instants**.

Three consequences follow from that one sentence, and they are the reason it is stated first:

1. **Trip access cannot outlive the share.** Every access query re-joins the live grant (`status = 'accepted' AND suspended_at IS NULL`) rather than trusting the participant row. Revoke the share and the trip grants nothing, on the next lookup, with no cleanup in the path (§3, §6).
2. **There is nothing to revoke when a trip ends.** The window closes because the clock passed an instant; no row is written and no grant is torn down.
3. **A participant is not a new kind of person on the car.** They are a share-holder whose grant means more for a while. `ResolveVehicleAccess` returns their **own share grant** alongside the elevated role — the trip elevates the ROLE, it does not replace the relationship, so a share-holder who can request rides keeps `allowRides` for the length of the window.

The participant resolution is **all-or-nothing and count-comparing**. Every requested share id must resolve to a live, accepted, unsuspended grant **on this vehicle**, and the check compares COUNTS rather than inspecting which id fell out: *"no such share"*, *"a share on somebody else's car"*, *"an invite never redeemed"* and *"a suspended grant"* are one answer (`400` + `participant_not_shared`), because reporting which would make the endpoint an oracle for other people's share ids. A create that silently dropped one invitee would produce a trip the owner believes has four people on it and that has three.

## 2. Window semantics

**The window is `[startsAt, effectiveEnd)` — start INCLUSIVE, end EXCLUSIVE — where `effectiveEnd = min(endsAt, endedAt)`.**

The predicate is spelled ONE way everywhere: `starts_at <= NOW() AND NOW() < COALESCE(ended_at, ends_at)`. It appears in `store.Trip.StatusAt`, in `telemetry.tripStatusOf`, in the SQL status filter, in the overlap probe, in the catalog's `activeTripId` resolver and — character for character — in `internal/auth`'s fourth UNION leg. **A surface that called a trip active while the access query called it over would render a live card over a socket that had already dropped the car.** The agreement is asserted rather than assumed (`TestTripStatusFilterAgreesWithStatusAt`).

**`status` is DERIVED and NEVER STORED**, and the reason is worth having in one place: **a stored status is the one state the platform could not explain** — a row saying `active` because a sweeper pass was missed, on a window that closed an hour ago. The sweeper's `started_notified_at` / `ended_notified_at` stamps record what was **NOTIFIED**, never what is **TRUE**. A trip whose window opened while the server was down is `active` the moment it comes back, with the stamp still NULL, and the sweeper then sends the push it owes.

**`endsAt` and `endedAt` are separate facts.** `endsAt` is the owner's **stated** close; `endedAt` is when they ended it early. The effective end is computed in every reader rather than written back over `endsAt`, because collapsing the two would destroy the owner's stated intent and make an accidental "End trip" unexplainable afterwards. It is also what keeps `POST /end` and `PATCH {endsAt}` from being the same button: `PATCH` refuses an `endsAt` in the past outright, because ending retroactively is a different operation with a different name.

**A window MAY start in the past**, deliberately unchecked. It is how the drives of a road trip **already driven** join the trip: drive selection is **WINDOW-based, not TAG-based**, so nothing is written at drive time, a trip created after the fact picks up the drives already recorded, and extending a window backfills automatically. This is also why the overlap probe's third predicate (`NOW() < COALESCE(ended_at, ends_at)`) is load-bearing: **only scheduled-or-active trips can conflict**, because a new backfilling window will routinely cover instants that old finished windows also covered, and history does not reserve the calendar.

**The 30-day cap** (`go_trips_window_capped`, `store.MaxTripWindow`) is what stops a mistyped year handing out a decade of a standing live-location grant. It is measured from `startsAt`, on create and on patch alike, and it is stated **twice** — in the CHECK constraint and in `validateTripWindow`. The duplication is deliberate: the constraint is the one that cannot be bypassed by a future writer, and the Go copy is what lets the API answer `400` with a sentence a person can act on rather than `500` with a constraint violation. **A validation that lives in one of two writers is a validation that holds half the time.**

## 3. The four access legs

`internal/auth`'s `queryUserVehicleIDs` is the one query every surface resolves a vehicle access set through — the WebSocket subscribed set, `GET /api/vehicles`, and every per-vehicle handler. It now has four UNION arms:

```sql
SELECT "id" FROM "Vehicle" WHERE "userId" = $1                      -- 1. OWNER
UNION
SELECT vehicle_id FROM go_vehicle_shares                            -- 2. ACCEPTED SHARE (MYR-184)
WHERE accepted_by_user_id = $1 AND status = 'accepted' AND suspended_at IS NULL
UNION
SELECT r.vehicle_id FROM go_ride_members m                          -- 3. LIVE RIDE MEMBER (MYR-540)
JOIN go_ride_requests r ON r.id = m.ride_id
WHERE m.user_id = $1 AND r.status NOT IN ('completed', 'declined', 'cancelled')
UNION
SELECT t.vehicle_id FROM go_trip_participants p                     -- 4. OPEN TRIP WINDOW (MYR-602)
JOIN go_trips t ON t.id = p.trip_id
JOIN go_vehicle_shares s
  ON s.vehicle_id = t.vehicle_id AND s.accepted_by_user_id = p.user_id
 AND s.status = 'accepted' AND s.suspended_at IS NULL
WHERE p.user_id = $1 AND p.left_at IS NULL
  AND t.starts_at <= NOW() AND NOW() < COALESCE(t.ended_at, t.ends_at)
```

**THREE PREDICATES ON THE FOURTH LEG, EACH LOAD-BEARING:**

- **THE WINDOW** — half-open, matching `StatusAt` exactly, with `COALESCE` making the owner's early end take effect here with no second column to read. **Access is purely the window** (client ruling): not a leg, not the car's status, not a destination.
- **THE MEMBERSHIP** — `left_at IS NULL`. A participant who left, or whom the owner removed, drops out on the next lookup with nothing to sweep.
- **THE LIVE SHARE JOIN** — the participant must **STILL** hold a live accepted, unsuspended grant on that same car. **This is what makes "trip access can never outlive the share" STRUCTURAL rather than a cleanup job.** It carries the same `status = 'accepted' AND suspended_at IS NULL` pair as leg 2 — one more copy of the suspension predicate catalogued in `internal/store/vehicle_share_access_queries.go`.

**The failure directions are asymmetric and worth naming.** If the fourth leg were dropped, a participant's map goes dark mid-trip. If it were **widened** — by losing the window — a person keeps live GPS on somebody's car forever. **The window predicate carries the risk, and it has a test of its own.**

`queryActiveTripParticipation` is the **per-vehicle** form and carries the same predicates **character for character**, for the reason its ride-side sibling states: a set that admits a vehicle while the role resolution denies it produces a client that subscribes and then receives a deny-all projection — the worst of both answers.

**Role resolution is STRONGEST-FIRST, and the ORDER is the whole correctness of the narrowing.** `ResolveVehicleAccess` resolves owner → `trip_participant` → `ride_member` → `viewer` → denied, and **the two elevated probes run BEFORE the share is allowed to answer.** Every trip participant is by construction also a share-holder, so resolving the share first — as the function did before MYR-602 — would return `RoleViewer` for every participant and **the window would grant nothing at all**. The share is still read first because it carries the capability flags; a separate `held` boolean distinguishes *"no grant"* from *"a live grant with no flags"*, which `ShareGrant`'s zero value cannot, and collapsing that distinction would admit every authenticated stranger as a viewer on every vehicle.

Both elevated probes **fail closed by returning false rather than an error**: they run on a path that has a correct answer without them (the share tier, or a denial), so a database blip must narrow the request rather than turn it into a `500` — and must never widen it.

**⚠ A TRIP ENDS WHEN THE CLOCK PASSES AN INSTANT. NOTHING WRITES A ROW.** That is the one way this leg differs from its ride-side sibling, and it matters operationally: a ride ends when a status changes, which is a mutation a revocation nudge can hang off. A trip's end is not a mutation, so **the 60-second `AccessRevalidator` sweep (`internal/ws/access_revalidator.go`, `DefaultRevalidateInterval`) is the enforcement** for an already-connected socket. The bound on a single machine is one interval; on any other machine the cached access set lapses on its own TTL. See §10's re-mask: the sweep now re-resolves per-vehicle ROLES rather than only membership, so a participant whose window closed is narrowed to `viewer` **in place** — their share keeps the vehicle in the access set, so the kick path never fires and never would have. The sweeper nudges the revalidator on every transition, because both run at 60 seconds and an unnudged opening would leave a socket masked as a plain viewer for nearly a minute after the phone buzzed to say the trip had started.

## 4. What a trip ADDS over a share

**An accepted share buys a catalog row and, at the top tier, the right to REQUEST rides.** Since [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369) it buys no drive history. **And since MYR-602 it carries no live location and no navigation at all** — the narrowed `viewer` answers *which car is this, can it make the trip, is it available*, and nothing about where it is.

**A trip adds, for a bounded window, to a chosen subset of those share-holders:**

1. **The location and navigation groups** — the `trip_participant` mask role, byte-for-byte the `ride_member` field list, which is byte-for-byte the **pre-MYR-602 viewer set** (`vehicleStateLiveViewerFields`, pinned by `TestLiveRolesMatchThePreMYR602ViewerSet`).
2. **`VehicleSummary.location`** on the catalog row — the coordinate the client's per-row pickup ETA is routed from.
3. **The window's DRIVES** — §7.2, §7.3 and §7.4, otherwise **owner-only since MYR-369** (§5).
4. **The per-leg Live Activity**, started by the server through a push-to-start token, because a leg begins while the app is not running and ActivityKit does not let a background app start an Activity.
5. **The five `trips` pushes** — `trip_added`, `trip_started`, `trip_ended`, `trip_leg_started`, `trip_leg_arrived`.

**THE MYR-435 CORRECTION, RECORDED HONESTLY.** MYR-435 (client decision, 2026-08-02) rebuilt the viewer allow-list as an explicit allow-list and **deliberately kept the navigation group**, on the stated ground that *"where is this car taking me is the other half of the shared-viewing feature"*. **That decision was not wrong when it was made and it is not in force now.** The client's 2026-09-05 ruling supersedes it: the MYR-435 argument is about **a party who is watching a journey**, and a **standing share is not that party** — it is durable and remote, and most of the time the grantee is at home while the owner drives alone. Being on a ride, or inside a trip window the owner opened, **is** that party. Nothing was deleted: every field MYR-435 kept still reaches a `ride_member` and a `trip_participant`, which is why none of them appears in `vehicleStateOwnerOnlyFields`.

**The narrowing had a contract consequence that is documented in `rest-api.md` §5.1.1 and must not be re-derived here:** six of the withheld fields are in `vehicle-state.schema.json`'s `required` array, so they are emitted as the schema's own no-value spellings (`0,0` = no fix; `""` = no geocode) rather than removed — because removing a required key does not narrow a frame, it makes it **undecodable** for every installed build. **A consumer cannot tell "withheld" from "no fix" by reading the value and MUST branch on the role it holds.**

`destinationAddress` is in the elevated list, and MYR-602's own spec text originally said it should not be (*"name + coordinates suffice; address stays owner-only"*). It was corrected in review for a plain reason: **a ride member receives it today**, and a trip participant shares that list by construction, so excluding it would have made a trip participant see LESS than the pre-MYR-602 viewer they replace — a narrowing nobody asked for, dressed as a widening.

## 5. The drives bound

**A trip is the FIRST and ONLY thing that lets a non-owner read a vehicle's drives.** MYR-369 made drive history owner-only and that still stands for `viewer` and for `ride_member`, whose mask entries on the drive resources are unreachable by routing.

**The admitting windows are ACTIVE **or** ENDED, never SCHEDULED**, which is wider than the live-access predicate and deliberately so:

- **Ended windows are included** because live location is a window-scoped grant that ends with the window, whereas **the window's DRIVES are the record of a journey the person was actually part of** — and having the list go dark at the moment the trip ends would delete the feature exactly when it becomes worth reading.
- **Scheduled windows are excluded** because a window that has not opened contains no drives, and **admitting one would let an owner grant read access to the PAST by scheduling a trip for next week.**
- The live-share join still applies, so a person whose grant is revoked loses the drives with everything else.

**THE LIST NARROWS IN THE STATEMENT, NOT BY FILTERING A PAGE.** `queryVehicleDrivesInTripWindows` unnests the caller's windows as two parallel `timestamptz[]` arrays into a derived table inside the `WHERE`. Filtering after pagination is how a page of ten becomes a page of two while eight matching drives sit behind the cursor — the narrowing has to happen in the statement that applies the `LIMIT`, so it has to happen in the store. Two further properties fall out of the array shape and both are deliberate: the statement stays a `const` with a **fixed parameter count**, so it cannot be built wrong for a caller with three windows; and **there is no string concatenation anywhere near an access-control predicate**. **A caller with no windows matches nothing structurally** — unnest over two empty arrays yields no rows — and the handler refuses before reaching the statement anyway, which is a gate that survives a refactor of the layer above it.

**The `startTime` bound is compared as an INSTANT, not as a string.** `Drive."startTime"` is a Prisma-owned TEXT column holding RFC 3339, and text ordering matches chronological ordering only while every row carries the same offset spelling. §7.2's cursor already relies on that, and a wrong answer there is a pagination glitch; **here a wrong answer is an access decision** — a participant reading a drive from outside their window — so the bound is cast. The cursor comparison stays TEXT, byte-identical to §7.2's, so the owner's list and the participant's narrowed list page the same way.

**Both bounds are INCLUSIVE, where the access predicate's upper bound is exclusive.** The asymmetry is deliberate and the two answer different questions: **access** is about a live socket AT an instant, so at exactly `effectiveEnd` the frame must not be delivered; **a drive** is a thing that happened, and one that began exactly at the closing instant **is a drive of this trip** — excluding it would lose it from the only list it belongs to.

**THE SINGLE-DRIVE REFUSAL IS `404`, NOT `403`, AND ONLY FOR THIS ROLE.** A caller with no trip window at all is asking *"may I read this car's history"*, and that stays `403 vehicle_not_owned` — the pre-MYR-602 behaviour, on a surface this issue is not about. A caller who **is** a participant and asked for a drive outside their window gets `404`: **the window is the entire extent of what they were told about this car, and a `403` would confirm the car made a journey on a day they were not part of.** `CoversDrive` is written as a fold over `TripDriveWindows` rather than as its own `EXISTS`, so the set that admits a LIST and the set that admits a DETAIL are provably one set — two statements would be two chances to write the predicate differently.

**The bound is enforced at the handler and in the statement, NEVER by the field mask.** The mask gives `trip_participant` the same drive fields an owner sees, and that is correct: what differs between the roles is **which drives they may ask for**, never what a drive says once they may see it. **A mask can hide a field and cannot hide a row.**

## 6. The revoked-share cascade is COSMETIC

When a grant is revoked or handed back, `TripRepo.RemoveParticipantsForShare` stamps `left_at` on that person's memberships in that car's **non-ended** trips.

**⚠ THIS IS NOT WHAT ENFORCES THE ACCESS RULE, and mistaking it for that would be the dangerous reading.** Trip access cannot outlive the share because **every access query re-joins the live grant** — the fourth UNION leg, `queryActiveTripParticipation`, `queryTripWindowsForUserVehicle` and the catalog leg all carry `status = 'accepted' AND suspended_at IS NULL`. **A revoked grant stops granting the instant it is revoked, cascade or no cascade, and if this function never ran the security property would still hold.**

**What it fixes is the ROSTER.** Without it the owner's trip card keeps listing somebody who can no longer see anything, the participant count lies, and the person appears in the "who is on this trip" list of a car they have been removed from. It is a **display-consistency repair**, and it is written down as one — here, in the function's own doc comment, and in `data-classification.md` §1.26 — **so nobody later deletes an access predicate on the strength of it.**

It is keyed on `(vehicle, user)` rather than on the share id, so a grant revoked and re-issued under a new id still removes the person from trips they joined under the old one. It is scoped to trips that have **not ended**: rewriting a finished trip's roster would rewrite history for no benefit — the window is closed, the access is already gone, and the roster is the only record of who was on it.

## 7. Concurrency

**`Create` and `Update` each take a transaction advisory lock on the VEHICLE before the overlap probe** (`pg_advisory_xact_lock(hashtext('go_trips:' || vehicleID))`).

**The probe alone is not enough, and this is the whole reason the lock exists.** The overlap probe is a **READ** and the insert is a **WRITE**. Two concurrent creates on one car would both find no overlap and both commit — producing **exactly the double window the `409` exists to prevent**, and therefore two `activeTripId` candidates on one row where the contract promises one.

The lock is **transaction-scoped**: released by the commit or the rollback, with nothing to unlock and no way to leak one. It is keyed by `hashtext` over the cuid rather than by a real id, because advisory locks are keyed by integer and the key space is ours; a collision between two vehicles costs a moment of serialisation on an operation that happens a handful of times a day. It touches no other vehicle.

**The whole create is ONE transaction** — lock, probe, resolve participants, insert, admit — because **a trip that exists with an empty roster is worse than no trip**: the owner sees a live window they believe they shared and nobody has access to it.

Two smaller concurrency properties, both guard-based rather than lock-based, because they are single statements:

- **`POST /end` is guarded on `ended_at IS NULL`.** A second call updates zero rows and the endpoint re-reads and answers `200`. **Re-stamping would move the end FORWARD on every retry — which, for an access boundary, means a double-tap silently extending somebody's live location by however long the two taps were apart.**
- **`DELETE …/participants/me` is guarded on `left_at IS NULL`** and answers `204` either way.

Every owner-scoped mutation additionally carries `owner_user_id = $n` **in the statement itself**. The handler's ownership check produces the good error message; the predicate is what actually prevents one person mutating another's trip, applied by the database to the same row it writes, **so there is no check-then-write window**. A car that changed hands is exactly the case where the vehicle row and the trip row could disagree — and for a trip, the trip's own owner column is the right authority.

## 8. Encryption and classification

Two P1 columns, both **encrypt-only with no plaintext sibling** and both sealed by the same label encryptor MYR-447 introduced:

- **`go_trips.name_enc`** — the trip's name. **User content that routinely names a destination** (*"DFW → LA"*).
- **`go_trip_legs.destination_name_enc`** — a place a car actually drove to.

One P1 **capability**, deliberately **not** encrypted: **`go_trip_activity_tokens.push_to_start_token`**, on the reason `data-classification.md` §3.2 gives its two siblings — the sender must replay the exact bytes to Apple on every push, so there is no one-way form that works, and a round trip buys nothing against an attacker who cannot push without `APNS_KEY_P8`. **Log redaction is the control.**

**`NewTripRepo` PANICS on a nil encryptor.** That is not defensiveness: both encrypted columns are `NOT NULL` with no plaintext fallback, so an unconfigured encryptor would not degrade the feature — it would fail every write at the constraint and return ciphertext to the user. A composition-root mistake that would silently disable encryption on P1 user content must stop the process at boot, not surface during somebody's road trip. (Nil **metrics** are tolerated and substituted: their absence costs a dashboard.)

Everything else across the four tables is P0 — opaque cuids, instants, booleans. **The roster's names are not stored at all**; they are resolved at read time from §1.15's `label` and §1.20's confirmed-name gate, because a second, staler P1 copy of a person's name would come with its own deletion problem. Full details, per column, in [`data-classification.md`](../contracts/data-classification.md) §1.25–§1.28, plus the `go_live_activities.trip_leg_id` row added to §1.18.

## 9. Account deletion (step 8g)

**Four statements, one cascade.** Specified normatively in [`data-lifecycle.md`](../contracts/data-lifecycle.md) §3.1 step 8g and §1.4.7.

| Statement | What it removes |
|---|---|
| `DeleteTripsOwned` | `DELETE FROM go_trips WHERE owner_user_id = $1` — the windows this person **created** |
| `DeleteTripParticipations` | `DELETE FROM go_trip_participants WHERE user_id = $1` — their memberships of **other people's** trips |
| `DeleteTripActivityTokens` | `DELETE FROM go_trip_activity_tokens WHERE user_id = $1` — their push-to-start registrations on other people's trips |
| `DeleteTripLegActivities` | `DELETE FROM go_live_activities WHERE user_id = $1 AND trip_leg_id IS NOT NULL` — their **running leg cards** under other people's trips. The anchor predicate is load-bearing: without it the statement would also take their RIDE Activities, whose end push a different lifecycle is responsible for sending before the row goes |

**ONE STATEMENT FOR FOUR TABLES on the first row.** Migration 0047 declares real FKs from the three children to `go_trips(id) ON DELETE CASCADE`, so deleting the parent takes the roster, the tokens and the legs with it. **A hand-rolled four-statement version would be four chances to miss one, and the one it missed would be a dangling row in an access gate.**

**Why four statements and not one:** a person stands in two relations to trips and the deletions are not the same shape — windows they opened, and windows they were invited into. The third is separate again because **a token is a LIVE CAPABILITY on a phone, not a membership record**: a person may hold a push-to-start token for a trip they have already left, and a deletion that only walked the roster would leave it behind. Running it after the cascade finds fewer rows, never more, and finding none is the idempotency every step in the sequence has.

**Ordering:** after **step 3** (the per-vehicle teardown, which deletes a car's trips in its own transaction, so anything left here is what the teardown could not reach), and before the identity delete, like every destructive step. Nothing later in the sequence reads these rows.

**Four counts reach the `account_deleted` audit metadata** — `tripsDeleted`, `tripParticipationsDeleted`, `tripActivityTokensDeleted`, `tripLegActivitiesDeleted` — and they are **counts and never rows** (CG-DL-5): the trip name is P1 user content and both tokens are P1 capabilities, so the only thing that may cross that boundary is how many. Four rather than one because a deletion that reached some relations and not others is exactly the state they exist to make visible.

**WHAT DELIBERATELY SURVIVES: the DRIVES.** A trip never owned a drive — the window merely **selected** it — so closing the window changes nothing about the vehicle's own history, which the owner's vehicle teardown deals with on its own terms.

**Memberships are DELETED here, not tombstoned**, which is the one place that is right: everywhere else `left_at` answers *"was this person ever on the trip"*, and after an account deletion there is no person left for that question to be about.

## 10. Legs and the per-leg Live Activity

### What a leg is

One journey inside an open window: the car sets off for a named place, drives,
and arrives or parks. A leg begins when, during an active trip, the vehicle is
**driving WITH a non-empty `destinationName`**; it ends on arrival, on the route
being cleared, on the car parking short, or on the window closing underneath it.

At most **one leg is open per trip at a time**, enforced by a partial unique
index (`idx_go_trip_legs_open_per_trip`) rather than by the detector's care. The
detector is event-driven and its inputs can arrive twice — a redelivered frame,
two processes during a rolling deploy — and a second open leg would produce a
second Live Activity on every participant's lock screen for the same journey.

### Why the detector reads raw telemetry

**There is no "driving with a destination" event in this service**, and the two
ways of creating one were both rejected:

**`drive.started` (internal/drives) is a SHIFT-STATE machine.** It fires when
the car moves out of P, it carries a VIN, a drive id and a coordinate, and it
knows nothing whatever about navigation. Extending `DriveStartedEvent` with a
destination would make a state machine about GEAR depend on a field group it has
no reason to know, and would put a field on every existing consumer that only
this package reads. The disqualifying problem is **TIMING**: a driver commonly
sets the destination on the dash AFTER pulling out, so a destination sampled at
the drive-start instant is empty on a large fraction of real legs.

**`internal/arrival` is RIDE-SCOPED to its bones.** Its candidate set is built
from `go_ride_requests` rows with a dispatched pickup, and its verdict writes a
ride status.

So `internal/trips` subscribes to `TopicVehicleTelemetry` and keeps a **per-VIN
destination and motion cache**, opening a leg on the transition into "driving,
with a destination" **from either side**:

| Sequence | Real-world shape |
|----------|------------------|
| destination set → car starts moving | the driver planned before pulling out |
| car already moving → destination appears | the driver set the route on the dash afterwards |

The second is the common one and is precisely what a drive-start event could
never have expressed. Both are pinned by `TestLegOpens`.

**Cost.** This is the busiest topic in the service — up to one frame per second
per streaming car — so the per-frame path is a VIN lookup in a cached map and,
for the overwhelming majority of frames (cars in no open window), nothing else.
One frame per 15-second TTL pays for a query; only a genuine edge pays for a
write. The candidate cache fails towards **detect nothing** after four TTLs of
failed refreshes, because opening a leg on a closed window would push a card to
people whose access has already been revoked.

### Arrival evidence, and the one inversion against internal/arrival

`internal/arrival` refuses to treat the car's own `milesToArrival` as evidence,
and says why in as many words: the dash's target and the RIDE's target are
different facts, with MYR-527 as the standing proof — a rider spent three hours
on a trip whose car was still navigating to the pickup.

**On a trip leg that argument inverts, and the inversion is exact rather than
convenient.** A leg is DEFINED as "the car is driving to the place the dash
names", so the dash's target IS the leg's target by construction, and the
distance the car reports to its own destination is the most direct evidence
available. The destination COORDINATE (`destinationLocation`) is used when the
car streams one; the reported distance is the fallback.

Everything else is reused verbatim from `internal/arrival` — the **80 m radius**,
the **20-second dwell**, the **1 mph stopped threshold**, and the three-rung
stillness ladder including MYR-563's positional rung. Two sets of numbers for one
physical question is how two detectors end up disagreeing about the same car in
the same car park.

**A stop inside the radius is the beginning of an arrival, not the end of a leg.**
This is the first bug the detector's tests found: a car that arrives also stops,
so the park-short branch closed every successful arrival as `completed` one
second into a twenty-second dwell, and the arrival could never fire. The
park-short branch is now guarded on `!atDestination`.

**The residual case, stated honestly:** a car that parks at its destination and
then goes completely silent — with not even a MYR-394 REST poll frame to satisfy
the dwell — keeps its leg open until the window closes, and is then settled as
`completed`. That is the honest answer, since nothing ever proved it stayed, and
it is the same dependency `internal/arrival` has on the same poller.

### An empty destination name is not a cleared route (MYR-612)

**Tesla streams DELTAS, and one of them can carry an EMPTY destination name while
the car is plainly still navigating.** Production, 2026-09-08: a car four minutes
into a leg to a hotel in Sedona sent exactly that — an empty name, `status =
driving`, `minutesToArrival = 98`, and the dash still showing the place. The
detector read it as *"the driver cancelled navigation"*, closed leg A at
03:40:22, and opened leg B for the same journey at 03:40:24. **Two rows, two
`trip_leg_started` banners, two push-to-start fan-outs, and leg A's card ended as
`completed` on a lock screen while the car drove on.**

**A clear is now PENDING until something confirms it.** While it is pending the
remembered destination, its coordinate, the arrival latch and the dwell all
survive, and the card is deliberately not refreshed — the honest content-state is
the one already showing, and its stale-date keeps it honest. Three things
confirm:

| Confirmation | Why it needs no debounce |
|--------------|--------------------------|
| **Park** | A stopped car with no route is not on its way anywhere. The park-short branch would reach the same verdict a frame later regardless. |
| **Sustained** | `LegClearGrace` (60 s) has passed with **no name AND no arrival estimate**. Both, not either: a car still reporting how long it has to go still has somewhere to be, whatever a delta left out of one frame. On a genuine cancellation Tesla stops sending both, so the two go stale together. |
| **Arrival** | The strongest claim, checked first, and the only one that fires `trip_leg_arrived`. |

### Resuming a leg, and why it is a SECOND line rather than the first

Debouncing cannot prevent every wrong close: a process restart between the two
frames, two servers during a rolling deploy, a grace that expired one frame
before the name came back. So when a car sets off again for the **same place**
within `LegMergeWindow` (120 s) of closing a leg **without arriving**, that leg is
**RESUMED** rather than replaced — `store.ResumeRecentLeg`, served by
`idx_go_trip_legs_vehicle_ended` (migration 0049).

The three columns it touches are exactly the ones that describe an ending:

| Column | On resume | Why |
|--------|-----------|-----|
| `ended_at` | cleared | The leg is under way again. |
| `activity_ended_at` | cleared | Whatever ending was delivered is undone, so the leg's real ending can claim and send. |
| `activity_started_at` | cleared **only if a card was actually ENDED** | A card that was ended is gone from the lock screen and the resumed leg needs a new one; a card still running must not be started twice. |
| `started_notified_at` | **untouched** | The banner already went out for this journey and this is the same journey. The duplicate banner is what the incident was about. |
| `arrived` | n/a | `arrived = false` is a precondition of being resumable at all. *"Your car arrived"* cannot be taken back. |

**The destination is compared on the PLAINTEXT, in Go.** `destination_name_enc` is
sealed with a random nonce, so two seals of one name are different bytes and no
SQL predicate can match them.

### The four endings, and what each one sends

| Ending | `arrived` | `trip_leg_arrived` push | Final card status |
|--------|-----------|-------------------------|-------------------|
| Dwell satisfied at the destination | `true` | **sent** | `arrived` |
| Driver cleared the route | `false` | — | `completed` |
| Car parked short of the destination | `false` | — | `completed` |
| The trip's window closed underneath it | `false` | — | `completed` |

The distinction is load-bearing rather than cosmetic: *"your car arrived"* is a
sentence that cannot be taken back.

### The Live Activity

**The server creates the card.** A leg begins while no participant's phone is
doing anything, and iOS gives a backgrounded app no way to start an Activity, so
the card is raised by an ActivityKit **push-to-start** addressed by a token the
app registers once per trip. The full envelope, the `attributes-type` constant,
the three attributes, the content-state and the fifteen-minute expiration are
specified in `rest-api.md` **§7.21.7**. The attributes are **four** — `tripId`,
`vehicleId`, `vehicleName` and `legId` — and the last is not decoration: the
card registers its own update token against the LEG through the dedicated
`POST /api/trip-legs/{legId}/activity-token` (§7.21.7), and a device that was
asleep when the leg opened cannot derive which leg its card is for. It is
REQUIRED, not optional: the iOS struct declares it non-optional, so a payload
missing it fails the decode and no card appears — the deliberate direction,
because a card with no anchor could never be updated or ended.

**Two tables, two meanings of a `410`:**

- `go_trip_activity_tokens` holds a **push-to-start** token, which addresses the
  **APP**. A `410` means the app is gone; the row goes from *this* table.
- `go_live_activities` holds a per-Activity **update** token, which addresses
  **one running card**. A `410` means the card is gone and the app is fine.

Routing a push-to-start rejection to the ride path would delete nothing while
leaving a dead registration retried on every remaining leg.

**One table for both kinds of Activity.** `go_live_activities` gained a second
anchor (`trip_leg_id`, migration 0047) with a CHECK that exactly one is set. A
separate table would have needed its own ETA ticker, held-end machinery,
token-rotation upsert and 24-hour reaper — and MYR-418 is the standing evidence
that this surface has no failure signal, so a second wrong implementation would
look exactly like a working one from the server all the way to the logs.

**A leg that never got a token registration still gets its pushes.** The card is
a courtesy on top of the banners, not a precondition for them: a trip whose
participants are all on the web produces zero cards and five perfectly good
notifications.

**TWO SENDERS RAISE THE CARD, AND THE SECOND ONE IS WHY ANYBODY GETS ONE
(MYR-612).** The leg-open fan-out runs **once**, at the instant the leg opens,
over whatever tokens are registered then — while registering is what a phone does
when the `trip_leg_started` push **wakes** it, necessarily afterwards. Production,
2026-09-08: the only participant's token was written at 03:40:27 for a leg that
opened at 03:40:24. Nothing looked again, `go_live_activities` stayed empty, and
the trip ran the whole evening with no card for anybody.

So `POST /api/trips/{tripId}/activity-start-token` is itself an occasion to send:
if the trip has an open leg, that device gets its push-to-start before the `204`.
The two senders cannot double-send because both **claim** first:

```
go_trip_activity_tokens.started_leg_id      (migration 0050)
    the leg whose push-to-start was last sent TO THIS DEVICE

    UPDATE … SET started_leg_id = $leg
    WHERE (trip_id, user_id) = (…) AND started_leg_id IS DISTINCT FROM $leg
    RETURNING push_to_start_token, sandbox
```

`IS DISTINCT FROM` rather than `<>` because the column is NULL until the first
claim, and `NULL <> 'leg-x'` is NULL — which is not TRUE and would claim nothing
at all on the very first send. The statement RETURNS the token so the sender
pushes exactly the bytes it claimed; a rotation between a list and a send would
otherwise push a dead token and stamp the row as done.

**Why not `activity_started_at`?** That is a LEG-level claim — *"the fan-out has
run"* — and cannot answer the per-DEVICE question the catch-up asks. **A token
ROTATION resets the stamp** (the new token addresses a phone holding no card);
an idempotent re-POST of the same value, which an app does on every foreground,
does not. A send that fails for a reason that might not repeat **releases** its
claim, so a later attempt can retry; a `410` does not, because that verdict
deletes the row.

### What one frame costs

The detector subscribes to the busiest topic in the service, so what the frame
path does per frame is a design constraint rather than a detail. MYR-612 is the
standing evidence: a car with a leg under way ran **two** unbounded statements
per frame on the single bus goroutine — the open-leg read and the trip audience —
at up to one per second for four minutes, and a JWT existence probe belonging to
an unrelated HTTP request timed out against the starved pool and answered `401`.

- The **audience** is read at a leg EDGE, never on the frame path. `updateLeg`
  never needed it: the only field it read was the vehicle id, which the leg row
  already carries.
- The **open-leg read** is served from a per-car cache for `LegReadTTL` (5 s),
  invalidated by every write and keyed on the FRAME clock, checked from both
  sides so a backdated MYR-394 poll frame forces a re-read rather than extending
  the window.
- Every remaining read is bounded by `Config.Timeout` and every edge by
  `Config.EdgeTimeout`, which is what that field's documentation always claimed
  and did not deliver on this path.

`TestUnderwayFramesDoNotQueryPerFrame` pins the cost, because the property is
invisible to every functional test — which is how it regressed.

### The four claims

A leg has four independent deliveries, and each carries its own durable stamp on
`go_trip_legs`, because they are four separate deliveries with four separate
failure modes — an alert can succeed while a push-to-start fails, and each must
be retryable without re-sending the other:

| Stamp | Delivery |
|-------|----------|
| `started_notified_at` | the `trip_leg_started` banner |
| `activity_started_at` | the push-to-start fan-out |
| `arrived_notified_at` | the `trip_leg_arrived` banner |
| `activity_ended_at` | the alerting-update-then-end pair |

A fifth stamp lives on the OTHER table, because it answers a per-DEVICE question
these four cannot: `go_trip_activity_tokens.started_leg_id` (MYR-612), claimed by
both push-to-start senders. See The Live Activity, above.

### The window sweeper

Every 60 seconds, two claim statements — one per edge — each an
`UPDATE … RETURNING` against a stamped column. There is no read-then-write
anywhere in `internal/trips/sweeper.go`, which is the whole concurrency story:
two processes both run the pass and the stamps arbitrate.

**Endings are claimed first.** A trip whose entire window elapsed while the
process was down matches BOTH claims, and sweeping starts first would announce
*"your trip started"* and then, milliseconds later in the same pass, that it had
ended.

`Service.SettleTrip` is the single function every closing edge goes through: the
sweeper's ticker, the owner's early-end handler, and a window that elapsed
during a restart. It ends the open leg's card **before** the trip announcement,
because a card still saying "heading to the Grand Canyon" is a lie on a lock
screen while a missing banner is only a silence.

### The WebSocket re-mask

A trip window opens and closes on the **clock**, so unlike a revoked share there
is no mutation anywhere for the fast revocation nudge to hang off. The
60-second `ws.AccessRevalidator` sweep is therefore not a backstop for trips —
it **is** the mechanism, and it now re-resolves per-vehicle ROLES rather than
only membership:

- **narrowing** — a participant whose window closed still holds their share, so
  the vehicle never leaves their access set and the kick path never fires. They
  are re-masked to `viewer` in place: the connection survives, the location
  stops.
- **widening** — a share-holder whose window opened is promoted to
  `trip_participant` without reconnecting.

The sweeper nudges it on **every** transition, because both run at 60 seconds:
unnudged, a trip opening a moment after a sweep would leave every participant's
socket masked as a plain viewer for nearly a minute AFTER their phone buzzed to
say the trip had started — the push and the map disagreeing is the one failure a
person actually notices.

`Client.roles` is an immutable map behind an atomic pointer, replaced whole and
never edited, because it is read lock-free on the broadcast hot path.

## 10A. Deleting a trip (MYR-607)

**The owner may delete a trip in ANY status**, and the route is
`DELETE /api/trips/{tripId}` ([`rest-api.md`](../contracts/rest-api.md)
§7.30.10). It is the only mutation on this surface that can only ever REDUCE
what people can see, which is why it carries none of §7.30.4's `trip_ended`
refusal: that refusal exists because extending a lapsed window would resurrect
live access, and a deletion grants nothing to anybody.

**THREE PHASES, AND THE ORDER IS THE WHOLE DESIGN.**

| Phase | What happens | Where |
|---|---|---|
| 1 — end | `ended_at` is stamped, through the §7.30.5 statement reused verbatim (owner-scoped, guarded on `ended_at IS NULL`, a no-op on an already-ended trip) | `store.TripRepo.End` |
| 2 — settle | Every open leg is ended and its Live Activity ended; `trip_ended` is fanned out to the participants carrying **`deleted: true`**; the WS re-mask is nudged | `trips.Service.NotifyTripDeleted` |
| 3 — delete | Five statements in one transaction: the audit row, then the leg Activities, the legs, the tokens, the roster, the window | `store.TripRepo.Delete` |

**PHASE 2 READS ROWS PHASE 3 REMOVES.** Who is on the trip, which leg is open,
which device holds which card — after phase 3 nothing in the database can name
any of them. A settlement that ran last would end no card and tell nobody, and
every participant's lock screen would keep a live Activity for a journey that no
longer exists until ActivityKit's own 8-hour staleness ceiling retired it. That
is the same hazard the `0047` down-migration's header warns about, arrived at
from the other direction.

**PHASE 1 EXISTS ONLY FOR THE HALF-DONE STATE.** On the happy path the row is
deleted a moment later and nothing ever reads the stamp. Phase 2 takes every
card down and tells every participant the trip is over; **without `ended_at`
written first, a phase 3 that FAILED would leave the trip ACTIVE, so those
people would keep the car's live location for the rest of a window they had just
been told had closed** — told it ended, still watching. With it, the trip is
genuinely over, the client's retry deletes it, and the only artefact is a trip
that reads as `ended` on the owner's own list until then. The reverse ordering
cannot offer a conservative failure at all: its failure is a stranded card on
somebody else's phone that nothing can clear.

**`NotifyTripDeleted` IS `SettleTrip` WITH TWO DIFFERENCES**, and the second is
the one worth knowing: the banner carries the flag, and **the open legs are
closed even when the end claim was already spent.** `SettleTrip` returns early
on a lost claim because a second closing edge has nothing left to do; a deletion
is the LAST moment any of it is possible, so a settlement interrupted between
its claim and its legs (a redeploy, a crashed pass) is repaired here rather than
never. The claim still arbitrates the BANNER, which is what makes *"announce
only when the trip was scheduled or active"* hold without the handler passing a
status down — every path that ends a trip stamps `ended_notified_at`.

**`deleted: true` RIDES `trip_ended` RATHER THAN BEING A SIXTH EVENT.** Installed
builds switch on `event`, and a value they have never seen routes to their
default arm — *do nothing* for a lifecycle push — so a deleted trip would
silently stay on the phone of exactly the people it was deleted out from under.
The flag is a JSON boolean, absent (never `false`) on an ordinary end, so every
push that existed before MYR-607 is byte-identical.

**WHAT SURVIVES: the DRIVES**, for the reason §9 gives about account deletion —
a trip never owned a drive, the window merely selected it — and **the SHARES**,
because a trip creates no vehicle relationship and deleting one destroys none.
The participants keep the plain `viewer` role their accepted grant already gave
them.

**ACCESS STOPS WHEN THE ROWS DO.** The fourth UNION leg joins `go_trips`, so a
deleted trip grants nothing on the next lookup. A connected socket is re-masked
by the `AccessRevalidator`'s own 60-second tick rather than by phase 1's nudge —
that nudge is asked for while the rows still exist — which is exactly the
mechanism §10's re-mask section describes for a window that lapses on the clock.

**ONE AUDIT ROW, `trip.deleted`**, written inside the transaction and before the
deletes (CG-DL-3): `targetType='trip'`, the owner's id, and `{vehicleId}` as the
whole of its metadata. **It is the only record the participants have.**
Normatively specified in [`data-lifecycle.md`](../contracts/data-lifecycle.md)
§3.6 and §4.2.

## 11. Kill switch

`TRIPS_ENABLED` (default true) is read at **composition** time: false constructs
neither the sweeper nor the detector, so a disabled deployment costs nothing and
holds no state, and the trip endpoints answer `503`.

**What it does not do is revoke access already resolved.** That is derived from
the trip rows by a query this switch does not reach, so a participant inside an
open window keeps seeing the car until the window's own end — the safe direction
for a switch whose purpose is to stop the machinery rather than to punish the
people using it.

## 12. The seams, and what is still not built

**Both lanes are now in.** Everything §10 and §11 describe — the sweeper, the leg
detector, the per-leg Live Activity and its push-to-start, the `trips` push
category and the WebSocket re-mask — landed alongside the model, the REST surface
and the access resolution, and the seams below are what joins them.

**THE SEAM IS `telemetry.TripNotifier`** ([`internal/telemetry/trip_notifier.go`](../../internal/telemetry/trip_notifier.go)). It declares the **three REST-caused** events the handler package owns — `TripAdded`, `TripStarted` (only for a create that lands inside its own window), `TripEnded` (only for an owner's early end) — and deliberately not the two telemetry-caused ones, which belong to the leg detector. It is satisfied by [`cmd/telemetry-server/trip_notifier_adapter.go`](../../cmd/telemetry-server/trip_notifier_adapter.go), a composition-root adapter over `trips.Service`'s `NotifyTripAdded` / `NotifyTripStarted` / `NotifyTripEnded`: the two packages name the same three events differently and neither is made to import the other's vocabulary.

**NIL IS A NO-OP, NOT A FAILURE, and that is the load-bearing property.** The constructor substitutes `noopTripNotifier`, so the call sites carry no nil checks and a fourth event added later cannot forget one. **A deployment with no notifier wired creates trips that work perfectly and tells nobody** — because a push is an announcement about a state change, never the state change itself, so a notifier that is absent, slow or broken must not be able to fail a create. Implementations must not block the request: every method is called **after the commit** and its result is discarded, and there is no error return because there is no error the handler could act on.

**`TripEnded` IS `SettleTrip`, and no second seam was added for it.** A closing edge is more than an announcement — the open leg's Live Activity has to be ENDED before the trip banner goes out (§10), and only the live package knows how — so `trips.Service.NotifyTripEnded` is a one-line delegation to `SettleTrip`, declared as such on the live side. The owner's early end therefore reaches the full settlement through the notifier the handler already holds: one claim on `ended_notified_at`, every open leg's card ended, `trip_ended` fanned out, the sockets re-masked. **The degradation with no notifier wired is stated rather than assumed:** an early end still ends the trip row — the window closes, access stops on the next lookup — and the sweeper's own closing pass picks the settlement up on its next tick, because both paths go through the same claim.

Two smaller seams are wired the same fail-closed way and are worth knowing about: `TripDriveAdmitter` (nil ⇒ the drives endpoints stay owner-only, exactly as MYR-369 left them) and `TripVehicleLister` (nil ⇒ the catalog stays owner + shared + member). **A deployment that forgot to wire trips under-serves rather than over-shares.**

**Still not built, recorded so the gaps are documented rather than discovered:**

- **The trip card has no ticker, and this is now DOCUMENTED rather than merely true** (`rest-api.md` §7.21.7). Its ETA refreshes only on the frames the detector sees, so a car that goes quiet mid-leg — a tunnel, an underground car park, a Tesla that stops streaming — holds its last number until the card's 3-minute stale-date marks it stale. **The stale-date is what keeps it honest in the meantime:** ActivityKit greys a stale Activity itself, so a participant sees "this number is old" rather than a wrong number presented as current, which is the same bound the surface already relies on between two ordinary ticks. The ride path has `ActivityTicker` for exactly this; a leg-anchored equivalent is a small follow-up (`ActivitiesForLeg` is already the shape it needs).
- **`internal/telemetry` still has no auth middleware (DV-19)** — every trips handler repeats the bearer preamble, like every other handler on the surface.
- **`account_deletion_sequence.go` keeps growing** ([MYR-584](https://linear.app/myrobotaxi/issue/MYR-584) is the standing decomposition issue).

**Closed in the review round, and recorded because each was a claim this document used to make:**

- **The reaper knows about legs deliberately now.** It used to cover them *by accident* — same table, same column — and the accident was load-bearing in the wrong direction: nothing stamped `updated_at` on a leg row, so a card on a day-long drive had its registration hard-DELETED while it was still on the lock screen, taking the end push's only address with it. The fan-out now marks the rows Apple accepted, and a store test plants a 30-hour-old leg row and proves it survives the sweep.
- **`push.Activity` carries `TripLegID` in its own field.** The leg id used to ride the `RideRequestID` slot on the argument that nothing read it — true, and the kind of true that lasts until somebody adds a log line.
- **`ErrLiveActivityRideClosed` is `ErrLiveActivityClosed`.** Both anchors return it and the HTTP answer is identical; only the name lied.
- **`v1Roles` is derived from `auth.AllRoles`.** It listed two of four. The consequence was a map growth, but the declaration READS as an enumeration of the vocabulary and a reader would have believed it.
- **`data-lifecycle.md` carries step 8g**, its four `account_deleted` counts, a retention-table row and a new §1.4.7 — the Go comments cited a section that did not exist.
- **The per-Activity update token has a route.** `RegisterLegActivity` and `EndLegActivity` had no production caller at all, so the whole Live Activity half addressed zero rows: the server could raise a card and never update or end it. `POST`/`DELETE /api/trip-legs/{legId}/activity-token` (§7.21.7) are that route pair — a DEDICATED path rather than a leg id smuggled into §7.21.1's ride route, and one carrying no trip id because a leg belongs to exactly one trip — and the push-to-start's `attributes` now carry a REQUIRED `legId` so the device can name the anchor.

## 13. References

- **Wire contract:** [`rest-api.md`](../contracts/rest-api.md) §7.30 (the ten trip routes, the error table, `activeTripId`, the kill switch), §5 (the four roles), §5.1.1 (sentinel substitution), §5.2.0–§5.2.4 (the per-resource masks), §10 **DV-26**.
- **Schemas:** [`schemas/trip.schema.json`](../contracts/schemas/trip.schema.json), and the MYR-602 amendments to [`vehicle-summary.schema.json`](../contracts/schemas/vehicle-summary.schema.json) (`activeTripId`, the rewritten `location` RBAC text), [`vehicle-state.schema.json`](../contracts/schemas/vehicle-state.schema.json) (the per-field MYR-602 visibility notes) and [`live-activity.schema.json`](../contracts/schemas/live-activity.schema.json) (the `ride` \| `trip` kind).
- **Classification:** [`data-classification.md`](../contracts/data-classification.md) §1.25–§1.28, §1.18 (`trip_leg_id`), §3.1 (two new encrypted columns), §3.2 (the push-to-start token), §6 (counts).
- **Schema:** [`internal/store/migrations/0047_trips.up.sql`](../../internal/store/migrations/0047_trips.up.sql) — the header comment carries the four-table argument and the CG-DL-9 FK reasoning.
- **Store:** [`trip_types.go`](../../internal/store/trip_types.go), [`trip_view.go`](../../internal/store/trip_view.go), [`trip_validate.go`](../../internal/store/trip_validate.go), [`trip_queries.go`](../../internal/store/trip_queries.go) (every statement in one file, with the three invariants at the top), [`trip_repo.go`](../../internal/store/trip_repo.go), [`trip_repo_read.go`](../../internal/store/trip_repo_read.go), [`trip_repo_write.go`](../../internal/store/trip_repo_write.go), [`trip_repo_delete.go`](../../internal/store/trip_repo_delete.go), [`trip_repo_drives.go`](../../internal/store/trip_repo_drives.go), [`trip_repo_catalog.go`](../../internal/store/trip_repo_catalog.go), [`account_deletion.go`](../../internal/store/account_deletion.go) (step 8g).
- **Handlers:** [`trip_handler.go`](../../internal/telemetry/trip_handler.go), [`trip_detail_handler.go`](../../internal/telemetry/trip_detail_handler.go), [`trip_drives_handler.go`](../../internal/telemetry/trip_drives_handler.go), [`trip_activity_token_handler.go`](../../internal/telemetry/trip_activity_token_handler.go), [`trip_wire.go`](../../internal/telemetry/trip_wire.go), [`trip_errors.go`](../../internal/telemetry/trip_errors.go), [`trip_types.go`](../../internal/telemetry/trip_types.go), [`trip_notifier.go`](../../internal/telemetry/trip_notifier.go), [`trip_drive_access.go`](../../internal/telemetry/trip_drive_access.go), [`vehicles_list_trip.go`](../../internal/telemetry/vehicles_list_trip.go).
- **Access and masks:** [`internal/auth/role.go`](../../internal/auth/role.go), [`internal/auth/queries.go`](../../internal/auth/queries.go), [`internal/auth/vehicle_access.go`](../../internal/auth/vehicle_access.go), [`internal/mask/tables.go`](../../internal/mask/tables.go), [`internal/mask/sentinels.go`](../../internal/mask/sentinels.go), [`internal/mask/mask.go`](../../internal/mask/mask.go).
- **Composition root and config:** [`cmd/telemetry-server/wiring_trips.go`](../../cmd/telemetry-server/wiring_trips.go), [`internal/config/config.go`](../../internal/config/config.go) (`TripsEnabled`).
- **Neighbours:** [MYR-184](https://linear.app/myrobotaxi/issue/MYR-184) (shares), [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369) (drives went owner-only; grant capabilities), [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435) (the viewer allow-list this narrowing supersedes in part), [MYR-447](https://linear.app/myrobotaxi/issue/MYR-447) (the label encryptor), [MYR-515](https://linear.app/myrobotaxi/issue/MYR-515) (`VehicleSummary.location`), [MYR-540](https://linear.app/myrobotaxi/issue/MYR-540) (ride members), [MYR-583](https://linear.app/myrobotaxi/issue/MYR-583) (the confirmed-name gate the roster reads), [MYR-602](https://linear.app/myrobotaxi/issue/MYR-602), [MYR-607](https://linear.app/myrobotaxi/issue/MYR-607) (the owner's delete).

### The code map

| Concern | File |
|---------|------|
| Package rationale, both rejected alternatives | `internal/trips/doc.go` |
| Window sweeper | `internal/trips/sweeper.go` |
| Leg detector (I/O half) | `internal/trips/detector.go` |
| Leg detector (pure rules) | `internal/trips/detector_state.go` |
| Leg lifecycle and its four claims | `internal/trips/legs.go` |
| The two window transitions, `TripNotifier`, `SettleTrip`, `NotifyTripDeleted` | `internal/trips/transitions.go` |
| The owner's deletion (handler / store) | `internal/telemetry/trip_delete_handler.go`, `internal/store/trip_repo_delete.go` |
| Trip banners (`trips` category) | `internal/push/notifier_trips.go`, `copy_trips.go` |
| Trip Live Activity | `internal/push/activity_trip_notifier.go`, `activity_trip_fanout.go`, `activity_trip_state.go` |
| Push-to-start envelope | `internal/push/activity_apns.go` |
| Live-side repositories | `internal/store/trip_live_repo.go`, `trip_leg_repo.go`, `trip_activity_token_repo.go`, `live_activity_trip_anchor.go` |
| WebSocket re-mask | `internal/ws/access_revalidator.go`, `client.go`, `hub_location_frames.go` |
| Composition | `cmd/telemetry-server/wiring_trips_live.go` |
