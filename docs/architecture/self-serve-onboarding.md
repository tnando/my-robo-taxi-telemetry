# Fully self-serve Tesla-owner onboarding (MYR-257)

**Status:** Design (Phase 1) + implementation (Phase 2, Option A). Supersedes the
ops-driven MVP in [`owner-onboarding.md`](owner-onboarding.md) §7 "Exact manual
steps required from Thomas" — those steps are what this issue deletes.

**Goal:** a brand-new person signs in with Apple, links their Tesla in-app, and
becomes a working owner (their car streams live telemetry) with **zero manual /
ops steps** — no `AUTH_APPLE_BOOTSTRAP` edit, no web Google sign-in, no seeded
`Account` row, no `ops fleet-config push`, per new owner.

**Hard constraints (from the client):**

1. The auth/provisioning path is **backend-owned**. The Next.js web app is NOT
   in the path (explicitly rejected). No step may require the tester to visit
   `myrobotaxi.app`.
2. Fully self-serve: no per-owner ops action of any kind.

---

## 1. The blocker (verified)

- Apple Sign-In (`internal/identity`) mints an Apple-native user in the Go-owned
  `go_users` table when the sign-in is genuinely new (`linkage.go`
  `resolveFirstSignIn` precedence **(4)**). The JWT `sub` the app then carries is
  that `go_users` id.
- Tesla ownership is **Prisma-owned**: `Account.userId`, `Vehicle.userId`, and
  `Settings.userId` are all FKs to `"User"."id"`. A `go_users` id is **not** a
  row in `"User"`, so the in-app Tesla link
  (`/api/tesla/link/callback` → `AccountRepo.UpdateTeslaToken`, an
  `UPDATE ... WHERE "userId"=$1 AND "provider"='tesla'`) has no `Account` row to
  attach to and returns `account_not_provisioned`.
- Today a human closes that gap by pre-creating the Prisma `User` (+ binding
  Apple → that cuid via `AUTH_APPLE_BOOTSTRAP`) before the tester can link. **That
  manual step is what must die.**

`internal/identity` deliberately never writes a Prisma table (ADR-001 §4; the
`go_` namespace + read-only `"User"` email lookup). That principle stays intact —
the provisioning writer is a **separate, sanctioned component**, not the identity
module.

---

## 2. Options considered

### Option A — sanctioned provisioning writer, gated behind a completed Tesla link (CHOSEN)

A new, narrowly-scoped writer creates the **minimal** Prisma rows for a
go_users-native owner **only after** they have proven Tesla ownership (the OAuth
code→token exchange has succeeded). It is triggered inside the
`/api/tesla/link/callback` success path.

**Why it respects the ownership rules:**

- **CG-DL-9 (letter):** CG-DL-9 governs *Go migration SQL* — no `internal/store/migrations/*.sql`
  may reference a Prisma table. This design adds **no migration** that touches a
  Prisma table (the only new Go-owned table, if any, is `go_`-prefixed). The
  User/Settings/Account rows are written by **application runtime SQL**, exactly
  the class of write `AccountRepo.UpdateTeslaToken` (Prisma `Account`) and
  `AuditRepo` (Prisma `AuditLog` insert-only) already perform under contract.
- **ADR-001 §4 (spirit):** the identity module still never writes Prisma. The
  writer lives in `internal/store` as a **distinct, single-purpose type**
  (`OwnerProvisioner`), separate from `AccountRepo`, invoked from the teslalink
  callback wiring — not from `internal/identity`.
- **Gated by proof of ownership:** no Prisma row is created until Tesla has
  returned tokens for the caller. A failed/denied link creates **nothing**
  (no orphan `User`).
- **Single, audited, idempotent, transactional path:** one method, one DB
  transaction, all upserts. Re-link never duplicates.

### Canonical-user resolution (the core correctness invariant)

Provisioning first resolves the **one** canonical Prisma user for the linked
Tesla account, *inside the transaction, before any write*, in this precedence —
converging identities rather than creating duplicates or reassigning ownership:

1. **(a) The Tesla `Account`(provider='tesla', providerAccountId=<sub>) already
   exists** → the target IS its `userId`. The Tesla account's owner is
   authoritative: **`Account.userId` is never rewritten.** If the caller's
   identity differs, the caller's Apple binding is *converged* onto that owner
   (a `go_identity_apple` re-point — see below).
2. **(b) A `"User"` with our Apple-VERIFIED email exists** → adopt it (reuse its
   id, converge the Apple binding). **No `"User"` INSERT.** The match email is
   strictly our own Apple-verified value (from the identity pipeline —
   `go_identity_apple`/`go_users`, only ever populated when Apple asserted
   `email_verified`). The **Tesla-account email is NEVER a match source**: an
   identity merge onto an existing `"User"` must not rely on an address we did
   not verify ourselves. If Apple hid the email (relay / unverified), this path
   is skipped — the caller converges via the verified Tesla sub (a) or provisions
   a fresh user (c). This is also what avoids a `User.email @unique` collision for
   the returning-web-user case.
3. **(c) Otherwise** INSERT a fresh `"User"` with the caller's `go_users` id —
   now guaranteed collision-free on **both** id and email (neither matched above).

**Why id-reuse still holds for the common (c) case.** `go_users.id` and
`"User"."id"` are independent PKs; the same cuid may exist in both. Case (c)
reuses the go_users id as the `"User".id`, so the existing `go_identity_apple`
binding already resolves every later sign-in via precedence (1) — no mapping
table, no per-owner `AUTH_APPLE_BOOTSTRAP`. Cases (a)/(b) instead **converge**:
`UPDATE go_identity_apple SET user_id = <canonical> WHERE user_id = <caller>`.
This is the *one* sanctioned re-point of an Apple binding (ADR-001 §4 otherwise
freezes it), justified because completing the Tesla OAuth link — or an exact
email match — proves the two identities are the same human. The orphaned
go_users row left behind is harmless (no binding, no `"User"`).

**Why this is not a mapping table.** The convergence writes the canonical id
*into the existing binding*, so there is still exactly one authoritative
`apple_sub → user_id` row — no second row to drift. `ProvisionTeslaOwner` returns
the **canonical** user id; the caller uses it (not the input sub) for vehicle
sync, so a converged link never seeds vehicles under the wrong id.

### Option B — move `Account`/`Vehicle` ownership off `"User"` to `go_users`

Unify identity Go-side so `go_users` ids are first-class owners. Cleaner
long-term, but it is a **schema migration of the Prisma-owned `Account`,
`Vehicle`, `Settings` FKs** on the shared prod DB, coordinated across the Next.js
repo's Prisma schema. That is exactly the "large/risky migration" STOP case. Not
needed: Option A delivers full self-serve with no schema change. **Rejected for
now** (revisit only if the dual-id duplication in §2's insight ever becomes a
real problem — it does not at v1 scale).

**Decision: Option A, with canonical-user resolution.** No STOP condition is hit
— there is **no schema migration on the shared prod DB** (only runtime upserts
into existing Prisma tables + a `go_identity_apple` binding re-point).

---

## 3. Exact rows written

All writes happen in **one transaction**, only after a successful Tesla
code→token exchange, keyed by the **canonical** user id resolved in §2 (which may
differ from the caller's JWT sub on a converged link). `providerAccountId` is the
Tesla OIDC `sub` fetched from Tesla's userinfo endpoint with the fresh access
token (collision-safe against a later web link on the same Tesla account).

| Table (Prisma-owned) | Statement | Idempotency key | Columns written |
|---|---|---|---|
| `"User"` | resolve (a/b) or `INSERT ... ON CONFLICT ("id") DO NOTHING` (c) | `id` / email / tesla account | `id`, `name`, `email`, `updatedAt` (rest default) — **only in case (c)** |
| `go_identity_apple` | `UPDATE ... SET user_id=<canonical> WHERE user_id=<caller>` | `apple_sub` (binding) | converge on cases (a)/(b) only |
| `"Settings"` | `INSERT ... ON CONFLICT ("userId") DO UPDATE SET "teslaLinked"=true, "updatedAt"=now()` | `userId` = canonical | `id`, `userId`, `teslaLinked=true`, `updatedAt` (rest default) |
| `"Account"` | `INSERT ... ON CONFLICT ("provider","providerAccountId") DO UPDATE SET tokens…, "expires_at"=…` (**never `userId`**) | `(provider, providerAccountId)` | `id`, `userId`=canonical, `type='oauth'`, `provider='tesla'`, `providerAccountId`, `access_token`(+`_enc`), `refresh_token`(+`_enc`), `expires_at` |

- **Encryption:** `Account` tokens are dual-written (plaintext + `*_enc`
  AES-256-GCM) via the injected `cryptox.Encryptor`, identical to
  `AccountRepo.UpdateTeslaToken` (NFR-3.23/3.25, data-classification §3.3). No new
  plaintext credential surface.
- **`name`/`email` on `"User"`:** P1 PII. Sourced from the identity module's
  best-effort profile (the go_users / go_identity_apple row) — the `email` is our
  **Apple-verified** value only, never the Tesla-account email (which is used for
  display/projection elsewhere but is not a merge/persist anchor here). May be
  empty when Apple used a hidden relay. Never logged.
- **New cuids** for `Settings.id` and `Account.id` are generated Go-side (cuid
  shape, same generator class as `go_users` ids).

### Idempotency / concurrency

- **New owner (c):** fresh `"User"` + Settings + Account created.
- **Returning owner re-link (a):** resolves to self via the Tesla account;
  Settings stays linked; Account tokens refreshed in place. No duplicates.
- **Returning web user by Apple-verified email (b):** existing web user reused,
  Apple binding converged — **no colliding `"User"` INSERT**, one User row. Fires
  only on our verified email; an Apple hidden-relay caller (empty verified email)
  never adopts here even if a Tesla-provided email would have matched.
- **Cross-user relink (a, different caller):** existing owner kept,
  `Account.userId` **not rewritten**, caller's Apple binding converged onto the
  owner, only the owner's `Settings` flipped. Audited.
- **Concurrent re-link:** ON CONFLICT upserts are atomic at the row level in
  Postgres; two racing transactions converge. No dup, no deadlock (consistent
  table order).
- **Failure before token exchange** (denied / bad state / exchange error):
  provisioning is never called → **no orphan `User`**.

---

## 4. Vehicle sync (self-serve, no web app)

The MVP reused the Next.js `syncVehiclesFromTesla` to write `Vehicle` rows. This
issue adds a **server-side Go sync** so the web app is never in the path:

- `internal/teslafleet` gains a read-only `ListVehicles` Fleet call
  (`GET /api/1/vehicles`) using the just-linked access token.
- A `Vehicle` upsert (keyed on `teslaVehicleId @unique`, `userId` = provisioned
  cuid) writes the minimal identity columns (`teslaVehicleId`, `vin`, `name`,
  `model`, `year`, `setupStatus`) so telemetry auth (`queryUserVehicleIDs`) and
  the client vehicle list see the car. Live values (charge/GPS/status) are then
  filled by the streaming pipeline exactly as for the first owner.
- Triggered from the callback success path, after provisioning, best-effort: a
  vehicle-sync failure does **not** fail the link (the owner is provisioned; the
  sync retries on next link / on a dedicated re-sync). GPS columns are never
  written by the initial sync (no coordinates yet), so no half-pair `*Enc`
  invariant risk.

### 4.1 Cars the linker DRIVES but does not OWN — the filter became a consent gate (MYR-599)

**What this section replaced.** Finding 3 of this issue put an **ownership
filter** at the top of the provisioning loop: every Fleet-API vehicle whose
`access_type` was not `OWNER` produced an
`owner_vehicle_skipped reason=not_owner` audit line and nothing else. The intent
was sound — never act on somebody else's car — but the mechanism conflated two
different protections:

- *"Don't attach a car this person has no relationship with."* That is done by
  the **fleet listing itself**, which is scoped to the caller's own Tesla token,
  and by `UpsertOwnedVehicle`'s cross-user rule. **Neither consults
  `access_type`**, so the filter was not what was providing this guarantee.
- *"Don't ACT on a car this person does not own."* That one is real — and its
  answer is **consent, not absence**.

**The failure it produced.** On **2026-09-05** a tester ran "Add another Tesla"
for a car he drives on somebody else's Tesla account. OAuth completed, the token
was stored, he paired the virtual key in the Tesla app — and **no `"Vehicle"` row
was ever created**, so the app had nothing to show him and nothing to explain.
The filter was silent by design; the silence was the bug. The client's ruling
(Thomas, 2026-09-05) is that a driver MAY add the car, behind a pop-up in which
they acknowledge that the owner approved it.

**The replacement, stated as the gap between two sentences: the car IS
provisioned; nothing is pushed at it.** That gap is the whole feature — a
driver-linked car exists, can be named, can be seen, and keeps the virtual key
its linker may already have paired, while no fleet-telemetry config reaches it
from **any** path until the acknowledgment is recorded. Concretely, for a vehicle
whose Fleet listing reports a non-`OWNER` access level:

1. **It is provisioned exactly like an owned car.** The same `UpsertOwnedVehicle`
   with the same identity columns. **The tombstone rule and the cross-user rule
   are unchanged**: a VIN the owner deliberately removed is still skipped
   (`go_removed_vehicles`, MYR-261), and a `teslaVehicleId` already owned by a
   different user is still never reassigned.
2. **A `go_vehicle_driver_access` row is written** (migration **0046**;
   `OwnerProvisioner.RecordDriverAccess`) carrying Tesla's `access_type`
   **verbatim** — including the **empty string** older Fleet responses have
   shipped, which is treated as *driver*. **Fail closed: an unknown access level
   is never promoted to ownership**, and `''` is stored rather than an invented
   `"DRIVER"` precisely so the column can still answer *what did Tesla actually
   say?* months later.
3. **The fleet-config schedule is seeded with the outcome `awaiting_owner_ack`**
   and **no config is pushed**. This is the one schedule label in the system that
   describes a push that never happened: it is what lets the row say why it is
   sitting there, and what keeps the MYR-592 inactivity sweeper from
   "disconnecting" a car that was never connected.
4. **The log line is `owner_vehicle_driver_access`** (INFO, `user_id`, redacted
   VIN, raw `access_type`), **in place of**
   `owner_vehicle_skipped reason=not_owner`. Deliberately a different event name
   rather than a new reason on the old one: **a car was provisioned**, and any
   operator or query reading the old event as "nothing happened" would now be
   wrong.

**The two writes are ordered, and the order is normative rather than
incidental.** The driver-access row goes **first**, the schedule seed second,
because they carry different weights. The row is the **gate** — every push path
refuses a car that has one with a NULL `acknowledged_at` — so if it is missing
the car is indistinguishable from an owner's and the reconciler will configure it
on its next pass, silently, unattended, at a car whose owner agreed to nothing.
That is the only failure in this hook with a consequence outside our own user,
which is why it logs at **ERROR** while the seed's failure logs at **WARN**, and
why the setup-state derivation reads the **driver row** rather than the schedule
label: the authoritative fact must not be the one carried by the best-effort
write that is allowed to be absent. Neither failure fails the link — that is this
hook's standing contract.

**An OWNER re-link of a car that carries a driver row DELETES the row**
(`ClearDriverAccess`), which is the **access-upgrade** case: a title transfer, or
an owner who had been reaching their own car through a second account. The row is
evidence about a claim that is no longer true, and leaving it would keep the wire
saying `teslaAccessType: "driver"` about a car this person owns outright — and,
if it was never acknowledged, would hold the push gate shut on a car that needs
nobody's permission. **It does not run the other way, and that gap is recorded
rather than papered over: Tesla DOWNGRADING an owner to a driver is not
observed**, because nothing re-lists an already-provisioned owner's cars for that
purpose, so a car that changes hands at Tesla keeps streaming to its old linker
until they re-link or remove it. **Revoking driver-linked cars when Tesla removes
driver access is likewise explicitly out of scope for MYR-599.**

**What the row is, and what it is not: EVIDENCE, not permission.** The platform
cannot verify with Tesla that an owner approved anything — no Fleet API surface
exposes such a fact — so what is recorded is exactly what the platform actually
knows: **this person, at this instant, was shown this version of this text and
said yes**. That is what we would point to if an owner ever complained, and it is
the whole reason `acknowledgment_version` exists rather than a bare boolean:
copy changes, and an acknowledgment that cannot name what was acknowledged is
worth nothing. Classification is P0 in full —
[`../contracts/data-classification.md`](../contracts/data-classification.md)
§1.24.

---

## 5. Fleet-telemetry config push (self-serve stream start)

So a newly paired VIN starts streaming without `ops fleet-config push`:

- After vehicle sync, for each synced VIN the callback path triggers the existing
  `POST /api/fleet-config` server logic (`telemetry.FleetConfigHandler` /
  `pushFleetConfig`) so the car is told to stream to `telemetry.myrobotaxi.app`.
- **Virtual-key pairing stays a user action** (Tesla requires the owner to
  approve the key in the Tesla app — cannot be automated). The push is safe to
  attempt pre-pairing (Tesla no-ops / errors for an unpaired VIN); it is retried
  when pairing completes.
- **A DRIVER-LINKED CAR IS NEVER PUSHED AT UNTIL ITS ACKNOWLEDGMENT IS RECORDED**
  (MYR-599, §4.1). The gate is the car's `go_vehicle_driver_access` row with a
  NULL `acknowledged_at`, and it binds on **every** path, not just this one: the
  link-time hook above, the fleet-config reconciler (which excludes such cars
  from its candidate listing in SQL, via a partial-index-backed `NOT EXISTS`),
  `POST …/complete-setup`, `POST …/reconnect`, and both fleet-config push routes.
  **There is no path that pushes a config at a car whose owner nobody has claimed
  to have asked.** The gate opens on
  `POST /api/tesla/vehicles/{vehicleId}/acknowledge-owner-approval`
  ([`../contracts/rest-api.md`](../contracts/rest-api.md) §7.29), which records
  the acknowledgment and then performs the same best-effort push complete-setup
  performs. One question, asked seven ways in the codebase and answered by one
  predicate — `PendingAcknowledgment()` — so a gate spelled seven ways cannot
  drift into being spelled six.

### SAFETY — never push against a real car in dev/test

- The push is gated on a **runtime** predicate: it fires only for a real linked
  user with a real VIN when a proxy URL is configured (`TESLA_PROXY_URL`). In the
  absence of proxy config the hook is a no-op (same "warn + skip when
  unconfigured" pattern as the existing fleet-config endpoint mount).
- **Unit tests exercise the trigger logic with fakes only** — a fake fleet client
  records the call; **no test ever performs a live push**. There is no code path
  in which tests reach `auth.tesla.com` / the proxy.

---

## 6. Endpoint / contract impact

- `GET /api/tesla/link/callback` (`rest-api.md` §7.11.2): behavior change — the
  success path now **provisions** (User/Settings/Account upsert) instead of
  requiring a pre-existing `Account`. `account_not_provisioned` is no longer
  emitted for a genuinely new owner (kept as a defined reason for
  backward-compat; now unreachable on the happy path). New failure reason folds
  into `persist_failed` (userinfo or transaction failure). No new public
  endpoint, no new request/response shape.
- **Data-lifecycle §1.4:** `"User"` and `"Settings"` gain **Write (provisioning,
  insert/upsert only)** access for the Go server (previously "Read"/"None");
  `"Account"` write access widens from UPDATE to **UPSERT (insert on first
  link)**. This is a contract change → **`sdk-architect` review + `contract-guard`
  gate required** (touches Prisma-owned write surface + auth).
- **Data-classification:** no new persisted column. `Settings.teslaLinked`
  (P0), `User.name`/`User.email` (P1) already classified; the writer honors their
  log-safety (never logs name/email/tokens).
- **MYR-599 addendum (contracts v0.39.0), the one place this section's "no new
  endpoint, no new persisted column" claim stops holding.** The consent gate of
  §4.1 adds: **one new endpoint**,
  `POST /api/tesla/vehicles/{vehicleId}/acknowledge-owner-approval`
  ([`../contracts/rest-api.md`](../contracts/rest-api.md) §7.29 — the contracts
  package cites it as §7.24, which is taken; see that section's numbering note
  and DV-25); **one new Go-owned table**, `go_vehicle_driver_access` (migration
  **0046**, P0 in full,
  [`../contracts/data-classification.md`](../contracts/data-classification.md)
  §1.24); **one new wire field**, `teslaAccessType` (`"owner"` \| `"driver"`,
  OPTIONAL, absent reads as `"owner"`, both roles) on `VehicleSummary` (§7.0) and
  `VehicleState` (§7.1); **one new `setupState` member**,
  `awaiting_owner_acknowledgment`, which sorts before the other three because no
  push has been attempted at such a car; **one new audit action**,
  `vehicle.owner_approval_acknowledged`, metadata `{version}` only; and **one new
  account-deletion step**, 8f, whose audit metadata carries the count
  `vehicleDriverAccessRowsDeleted` and never the rows. Behaviour change on this
  document's own §7.11 callback path: the link hook no longer filters on
  `access_type`.

---

## 7. Cross-repo verification required (human / sdk-architect gate)

The provisioning INSERTs target the **prod Prisma schema**, whose authoritative
definition lives in the Next.js repo (not importable here). Before enabling this
in prod, `sdk-architect` must confirm against `prisma/schema.prisma`:

1. **`"User"."email"` is `@unique`.** This is load-bearing for the canonical
   resolver (§2): a naive `INSERT User ON CONFLICT ("id")` would still raise
   `23505` on the email index when a returning web user's Apple-verified email
   matches an existing row under a fresh go_users id. The resolver's precedence
   (a)→(b) **adopts** the existing email-owner instead of inserting, so the
   collision cannot happen. Note the match uses **only our Apple-verified email**
   (never the Tesla-account email), so a fresh-user INSERT (c) writes only a
   verified email (or NULL) into the unique column — it can never introduce an
   unverified Tesla email that later collides. Verify `@unique` still holds and
   that email is the only unique column the INSERT path could collide on.
2. `"Settings"."userId"` carries a UNIQUE constraint (the `ON CONFLICT ("userId")`
   target) and every other NOT-NULL `Settings` column has a DB-level `@default`.
3. `"Account"` has the compound `@@unique([provider, providerAccountId])` (the
   `ON CONFLICT` target) and no other NOT-NULL column without a default.
4. `"User"` NOT-NULL columns other than `id` (`updatedAt`) are set explicitly;
   the rest default. `"Vehicle"` NOT-NULL columns without a Prisma `@default` —
   confirmed to include `model`/`year`/`color`/`licensePlate` — are seeded with
   empty placeholders by `UpsertOwnedVehicle`; verify no other NOT-NULL
   Vehicle column lacks a default (or the identity-seed INSERT will error — a
   best-effort skip, but it means the car won't appear until the web sync runs).

**Cross-user semantics (enforced, not just documented).** The resolver never
reassigns ownership across users:

- **Tesla account already owned (a):** `Account.userId` is authoritative and is
  **never rewritten**. If a second identity links the same Tesla account, the
  existing owner keeps it; the caller's Apple identity is *converged* onto that
  owner via a `go_identity_apple` re-point (the one sanctioned re-point, gated by
  proof of Tesla ownership). Only the owner's `Settings.teslaLinked` is set — the
  caller's `Settings` is never created stale.
- **Vehicle already owned by another user:** `UpsertOwnedVehicle`'s
  `ON CONFLICT ("teslaVehicleId") DO UPDATE ... WHERE userId = EXCLUDED.userId`
  updates nothing (RowsAffected 0) → reported as `skipped_cross_user`, audited,
  and never pushed a fleet config. **This rule is untouched by MYR-599 and is the
  one that actually enforces cross-user safety** — it does not consult
  `access_type` and never did.
- **Vehicle the caller only DRIVES (Fleet `access_type != "OWNER"`): PROVISIONED
  BEHIND A CONSENT GATE, no longer filtered out** (MYR-599, contracts v0.39.0 —
  see §4.1 for the full argument). This bullet read *"Vehicles the caller only
  shares (Fleet `access_type != "OWNER"`) are filtered out before any write"*, and
  that filter is gone: it was silent, it left a tester with no `"Vehicle"` row and
  no explanation, and it was never what kept another person's car from being
  attached (the token-scoped fleet listing and the cross-user rule above do
  that). Such a car is now written like any other, plus a
  `go_vehicle_driver_access` row (migration **0046**) holding Tesla's
  `access_type` **verbatim** — an **empty** value counts as *driver*, failing
  closed so an unknown access level is never promoted to ownership — and its
  fleet-config schedule is seeded `awaiting_owner_ack` with **no config pushed**.
  The audit / log line is `owner_vehicle_driver_access` (INFO, redacted VIN) in
  place of `owner_vehicle_skipped reason=not_owner`. **Nothing reaches the car
  from any path** — link hook, reconciler, complete-setup, reconnect or a
  fleet-config re-push — until
  `POST /api/tesla/vehicles/{vehicleId}/acknowledge-owner-approval`
  ([`../contracts/rest-api.md`](../contracts/rest-api.md) §7.29) records that the
  linker acknowledged the owner's approval. An **OWNER re-link deletes** a stale
  driver row (the access-upgrade case); the reverse — Tesla downgrading an owner
  to a driver — is **not observed**, and revoking driver-linked cars when Tesla
  removes driver access is **out of scope for MYR-599**.
- **THE OWNER WINS A CAR A DRIVER PROVISIONED FIRST, and this bullet exists
  because the consent gate above created the problem it solves.** A driver's
  passive `AfterLink` now writes a `"Vehicle"` row, and that row claims the
  **UNIQUE `"Vehicle"."teslaVehicleId"`**. When the car's real owner linked
  afterwards their upsert failed the cross-user predicate, `VehicleSkippedCrossUser`
  was returned, and **their own car simply never appeared** — with nothing in the
  system able to fix it, because the losing party is the person who actually owns
  the vehicle. So an **OWNER-access link onto a driver-provisioned row TRANSFERS
  the row**, inside the same provisioning transaction that would otherwise have
  refused it: `"userId"` moves, the previous linker's driver-access row, fleet-config
  schedule and attempt history are deleted, and every share and invite they had
  issued against the car is revoked. The outcome is `VehicleOwnedByTransfer` and
  the hook logs `vehicle_driver_link_superseded_by_owner` at INFO — nothing
  failed, this is the designed resolution, but a car changed hands and the former
  driver was not told, so it is deliberately greppable.
  **THE MOVE IS ONE-WAY.** A DRIVER link onto an OWNER's row still skips exactly
  as it always did: owner wins in both directions, and there is no signal a driver
  can send that takes a car away from its owner. **AN OWNER-VERSUS-OWNER
  collision also still skips** — two accounts each claiming ownership of one
  `teslaVehicleId` is not something this rule can adjudicate, and guessing would
  be worse than refusing.
  **The audit row is filed under the FORMER DRIVER**, action
  `vehicle.driver_link_superseded_by_owner`, metadata `{"ownerUserId": …}` and
  nothing else: audit rows in this package name the person whose data the action
  was about, and the data that changed is theirs — a car left their list and their
  shares were revoked. The arriving owner's id rides in the metadata so the two
  ends of the move are joinable from either side. Two opaque cuids, no VIN (P1),
  no token, no share list.
- Every outcome (`new_user`, `adopted_by_email`, `adopted_by_account`,
  identity-converged, vehicle owned/skipped) emits a P0-only audit line (opaque
  cuids + opaque outcome; never email/name/tokens).

The Go tests recreate a **slim** schema fixture (same pattern as
`account_repo_test.go`) mirroring the classification tables — including the
`User.email` unique index and `go_identity_apple` — and exercise the crossover,
cross-user-relink, non-owner-vehicle, and concurrent-relink cases. A drift
between that fixture and prod Prisma is the risk this gate closes.

---

## 8. What still needs a human after this ships

- **One-time (already done for prod):** partner registration, telemetry keys +
  mTLS, signing virtual key in the proxy, Tesla-portal redirect URI, the
  `TESLA_LINK_*` + `AUTH_TESLA_*` + `ENCRYPTION_KEY` env. These are per-*deployment*,
  not per-owner.
- **Per new owner: nothing.** Apple sign-in → in-app Tesla link → tap the Tesla
  `_ak` virtual-key deep link (a Tesla-app action Tesla mandates the owner
  perform) → car streams. No ops step.

The only irreducible user action is approving the virtual key inside the Tesla
app — Tesla requires it and it cannot be automated by any backend.
</content>
