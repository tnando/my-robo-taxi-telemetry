# Trips — window-scoped live sharing (MYR-602)

**Status:** Implemented and merged on `myr602-core` — migration **0047**, `store.TripRepo`, `telemetry.TripHandler`, the fourth access leg in `internal/auth`, and the `viewer` narrowing in `internal/mask`. Contracts **v0.41.0** ([`schemas/trip.schema.json`](../contracts/schemas/trip.schema.json)). The wire contract is [`rest-api.md`](../contracts/rest-api.md) §7.30 and §5; the classification is [`data-classification.md`](../contracts/data-classification.md) §1.25–§1.28. **A sibling lane owns the sweeper, the leg detector, the per-leg Live Activity and the `trips` push category — see §10.**

**Goal.** Give an owner a way to say *"for these three days, these people may see my car"* — and, in the same change, take standing live location away from a share that is not saying that. The feature is one sentence: **a trip is a TIME WINDOW during which the share-holders the owner picked see the car's live location, its navigation, the window's drives and a per-leg Live Activity.**

**Client decisions (Thomas).**

- **2026-09-05, the narrowing:** *"you should really only see live location during an active trip shared with a user"* (or an accepted active ride). This **supersedes** [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435)'s 2026-08-02 decision to keep the navigation group on a plain `viewer` — see §4.
- **2026-09-05, access is the window:** a participant's access is **not** conditioned on a driving leg being underway, on the car's status, or on a destination being set. A parked car inside an open window streams to its participants exactly as it does to its owner. Legs govern the Live Activity and nothing else.
- **2026-09-06, the owner is in the Activity:** the owner may register a push-to-start token and is included in the per-leg Live Activity, even though the owner receives none of the three REST-caused pushes.

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

**⚠ A TRIP ENDS WHEN THE CLOCK PASSES AN INSTANT. NOTHING WRITES A ROW.** That is the one way this leg differs from its ride-side sibling, and it matters operationally: a ride ends when a status changes, which is a mutation a revocation nudge can hang off. A trip's end is not a mutation, so **the 60-second `AccessRevalidator` sweep (`internal/ws/access_revalidator.go`, `DefaultRevalidateInterval`) is the enforcement** for an already-connected socket. The bound on a single machine is one interval; on any other machine the cached access set lapses on its own TTL. See §10 — teaching the revalidator to **re-mask** rather than only to kick is the sibling lane's, and until it lands a role that narrows mid-connection is corrected by the sweep's existing behaviour rather than by an in-place re-projection.

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

**Three statements, one cascade.**

| Statement | What it removes |
|---|---|
| `DeleteTripsOwned` | `DELETE FROM go_trips WHERE owner_user_id = $1` — the windows this person **created** |
| `DeleteTripParticipations` | `DELETE FROM go_trip_participants WHERE user_id = $1` — their memberships of **other people's** trips |
| `DeleteTripActivityTokens` | `DELETE FROM go_trip_activity_tokens WHERE user_id = $1` — their push-to-start registrations on other people's trips |

**ONE STATEMENT FOR FOUR TABLES on the first row.** Migration 0047 declares real FKs from the three children to `go_trips(id) ON DELETE CASCADE`, so deleting the parent takes the roster, the tokens and the legs with it. **A hand-rolled four-statement version would be four chances to miss one, and the one it missed would be a dangling row in an access gate.**

**Why three statements and not one:** a person stands in two relations to trips and the deletions are not the same shape — windows they opened, and windows they were invited into. The third is separate again because **a token is a LIVE CAPABILITY on a phone, not a membership record**: a person may hold a push-to-start token for a trip they have already left, and a deletion that only walked the roster would leave it behind. Running it after the cascade finds fewer rows, never more, and finding none is the idempotency every step in the sequence has.

**Ordering:** after **step 3** (the per-vehicle teardown, which deletes a car's trips in its own transaction, so anything left here is what the teardown could not reach), and before the identity delete, like every destructive step. Nothing later in the sequence reads these rows.

**Three counts reach the `account_deleted` audit metadata** — `tripsDeleted`, `tripParticipationsDeleted`, `tripActivityTokensDeleted` — and they are **counts and never rows** (CG-DL-5): the trip name is P1 user content and the token is a P1 capability, so the only thing that may cross that boundary is how many.

**WHAT DELIBERATELY SURVIVES: the DRIVES.** A trip never owned a drive — the window merely **selected** it — so closing the window changes nothing about the vehicle's own history, which the owner's vehicle teardown deals with on its own terms.

**Memberships are DELETED here, not tombstoned**, which is the one place that is right: everywhere else `left_at` answers *"was this person ever on the trip"*, and after an account deletion there is no person left for that question to be about.

## 10. What this lane did NOT build

Recorded so the gap is **documented rather than discovered**. All of the following belong to a sibling lane working against the same base:

- **The trip sweeper** (`internal/trips` — the package does not exist on this branch): the boundary `trip_started` / `trip_ended` pushes at the instants no request is present for, and the `started_notified_at` / `ended_notified_at` stamps that make them at-most-once. The columns and the partial index `idx_go_trips_unswept` exist and are unread.
- **The leg detector**: everything that WRITES `go_trip_legs`. This lane's repository only READS legs (`queryTripOpenLeg`, for `Trip.currentLeg`); nothing here inserts, updates or ends one. The `arrived` evidence flag, the two leg push stamps and the two Activity stamps are unwritten.
- **The per-leg Live Activity push-to-start fan-out**: the sender that takes a `go_trip_activity_tokens` row and opens an Activity anchored on `go_live_activities.trip_leg_id`. The anchor column, its CHECK, its partial unique index and its live index all exist (migration 0047); nothing writes them yet.
- **The `trips` push category**: its [`rest-api.md`](../contracts/rest-api.md) §7.19 prefs toggle, its payload shape, its delivery flags and its deep link.
- **The WebSocket revalidator's RE-MASK.** `internal/ws` is untouched by this lane. The 60-second `AccessRevalidator` sweep already re-derives access sets and drops what no longer resolves; teaching it to re-project an already-connected socket at a narrowed role is the sibling lane's work. *(Note for the sibling: `internal/auth/queries.go`'s fourth-leg comment reads as though that re-mask has already landed. It has not — see §11.)*

**THE SEAM IS `telemetry.TripNotifier`** ([`internal/telemetry/trip_notifier.go`](../../internal/telemetry/trip_notifier.go)). It declares the **three REST-caused** events this package owns — `TripAdded`, `TripStarted` (only for a create that lands inside its own window), `TripEnded` (only for an owner's early end) — and deliberately not the two telemetry-caused ones.

**NIL IS A NO-OP, NOT A FAILURE, and that is the load-bearing property.** The constructor substitutes `noopTripNotifier`, so the call sites carry no nil checks and a fourth event added later cannot forget one. **A deployment with no notifier wired creates trips that work perfectly and tells nobody** — because a push is an announcement about a state change, never the state change itself, so a notifier that is absent, slow or broken must not be able to fail a create. Implementations must not block the request: every method is called **after the commit** and its result is discarded, and there is no error return because there is no error the handler could act on.

**Wiring it is ONE LINE** in [`cmd/telemetry-server/wiring_trips.go`](../../cmd/telemetry-server/wiring_trips.go): add `telemetry.WithTripNotifier(tripNotifier)` to the `NewTripHandler` option list.

Two smaller seams are wired the same fail-closed way and are worth knowing about: `TripDriveAdmitter` (nil ⇒ the drives endpoints stay owner-only, exactly as MYR-369 left them) and `TripVehicleLister` (nil ⇒ the catalog stays owner + shared + member). **A deployment that forgot to wire trips under-serves rather than over-shares.**

## 11. References

- **Wire contract:** [`rest-api.md`](../contracts/rest-api.md) §7.30 (the nine routes, the error table, `activeTripId`, the kill switch), §5 (the four roles), §5.1.1 (sentinel substitution), §5.2.0–§5.2.4 (the per-resource masks), §10 **DV-26**.
- **Schemas:** [`schemas/trip.schema.json`](../contracts/schemas/trip.schema.json), and the MYR-602 amendments to [`vehicle-summary.schema.json`](../contracts/schemas/vehicle-summary.schema.json) (`activeTripId`, the rewritten `location` RBAC text), [`vehicle-state.schema.json`](../contracts/schemas/vehicle-state.schema.json) (the per-field MYR-602 visibility notes) and [`live-activity.schema.json`](../contracts/schemas/live-activity.schema.json) (the `ride` \| `trip` kind).
- **Classification:** [`data-classification.md`](../contracts/data-classification.md) §1.25–§1.28, §1.18 (`trip_leg_id`), §3.1 (two new encrypted columns), §3.2 (the push-to-start token), §6 (counts).
- **Schema:** [`internal/store/migrations/0047_trips.up.sql`](../../internal/store/migrations/0047_trips.up.sql) — the header comment carries the four-table argument and the CG-DL-9 FK reasoning.
- **Store:** [`trip_types.go`](../../internal/store/trip_types.go), [`trip_view.go`](../../internal/store/trip_view.go), [`trip_validate.go`](../../internal/store/trip_validate.go), [`trip_queries.go`](../../internal/store/trip_queries.go) (every statement in one file, with the three invariants at the top), [`trip_repo.go`](../../internal/store/trip_repo.go), [`trip_repo_read.go`](../../internal/store/trip_repo_read.go), [`trip_repo_write.go`](../../internal/store/trip_repo_write.go), [`trip_repo_drives.go`](../../internal/store/trip_repo_drives.go), [`trip_repo_catalog.go`](../../internal/store/trip_repo_catalog.go), [`account_deletion.go`](../../internal/store/account_deletion.go) (step 8g).
- **Handlers:** [`trip_handler.go`](../../internal/telemetry/trip_handler.go), [`trip_detail_handler.go`](../../internal/telemetry/trip_detail_handler.go), [`trip_drives_handler.go`](../../internal/telemetry/trip_drives_handler.go), [`trip_activity_token_handler.go`](../../internal/telemetry/trip_activity_token_handler.go), [`trip_wire.go`](../../internal/telemetry/trip_wire.go), [`trip_errors.go`](../../internal/telemetry/trip_errors.go), [`trip_types.go`](../../internal/telemetry/trip_types.go), [`trip_notifier.go`](../../internal/telemetry/trip_notifier.go), [`trip_drive_access.go`](../../internal/telemetry/trip_drive_access.go), [`vehicles_list_trip.go`](../../internal/telemetry/vehicles_list_trip.go).
- **Access and masks:** [`internal/auth/role.go`](../../internal/auth/role.go), [`internal/auth/queries.go`](../../internal/auth/queries.go), [`internal/auth/vehicle_access.go`](../../internal/auth/vehicle_access.go), [`internal/mask/tables.go`](../../internal/mask/tables.go), [`internal/mask/sentinels.go`](../../internal/mask/sentinels.go), [`internal/mask/mask.go`](../../internal/mask/mask.go).
- **Composition root and config:** [`cmd/telemetry-server/wiring_trips.go`](../../cmd/telemetry-server/wiring_trips.go), [`internal/config/config.go`](../../internal/config/config.go) (`TripsEnabled`).
- **Neighbours:** [MYR-184](https://linear.app/myrobotaxi/issue/MYR-184) (shares), [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369) (drives went owner-only; grant capabilities), [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435) (the viewer allow-list this narrowing supersedes in part), [MYR-447](https://linear.app/myrobotaxi/issue/MYR-447) (the label encryptor), [MYR-515](https://linear.app/myrobotaxi/issue/MYR-515) (`VehicleSummary.location`), [MYR-540](https://linear.app/myrobotaxi/issue/MYR-540) (ride members), [MYR-583](https://linear.app/myrobotaxi/issue/MYR-583) (the confirmed-name gate the roster reads), [MYR-602](https://linear.app/myrobotaxi/issue/MYR-602).
