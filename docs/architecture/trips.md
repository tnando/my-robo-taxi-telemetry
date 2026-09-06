<!--
  MYR-602. THIS FILE HAS TWO OWNERS AND THE SECTION BOUNDARY BELOW IS THE SEAM.

  Everything above "## Legs and Live Activity" belongs to the trips CORE lane —
  the model, the REST surface, the access resolution, the drives window. That
  lane is landing on the same base branch and will fill in the sections it
  owns; this file is deliberately created with only the LIVE half written, so
  the two can land independently and neither has to rebase on the other's prose.

  If you are reading this comment in main, the merge of the two lanes has not
  been finished and the core sections above are still missing.
-->

# Trips

> **Status:** partial. This document currently carries only the LIVE half
> (`internal/trips`, the leg detector, the Live Activity). The model, the REST
> surface (`rest-api.md` §7.30), the access resolution and the drives window are
> owned by the trips core lane and land separately.

A **trip** is a `(vehicle, window, participant set)` tuple created by the
vehicle's owner. It creates no new vehicle relationship: every participant
already holds an accepted `go_vehicle_shares` grant on the car, and the trip
decides only what that grant MEANS between two instants.

**Access is purely the window** (client ruling, 2026-09-05). Inside it a
participant sees the car's live location and telemetry at all times — parked,
driving, with or without a destination. **Legs govern only the Live Activity and
the two leg pushes**, and nothing else in the system.

---

## Legs and Live Activity

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
specified in `rest-api.md` **§7.21.7**.

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

### Kill switch

`TRIPS_ENABLED` (default true) is read at **composition** time: false constructs
neither the sweeper nor the detector, so a disabled deployment costs nothing and
holds no state, and the trip endpoints answer `503`.

**What it does not do is revoke access already resolved.** That is derived from
the trip rows by a query this switch does not reach, so a participant inside an
open window keeps seeing the car until the window's own end — the safe direction
for a switch whose purpose is to stop the machinery rather than to punish the
people using it.

### Where the code is

| Concern | File |
|---------|------|
| Package rationale, both rejected alternatives | `internal/trips/doc.go` |
| Window sweeper | `internal/trips/sweeper.go` |
| Leg detector (I/O half) | `internal/trips/detector.go` |
| Leg detector (pure rules) | `internal/trips/detector_state.go` |
| Leg lifecycle and its four claims | `internal/trips/legs.go` |
| The two window transitions, `TripNotifier`, `SettleTrip` | `internal/trips/transitions.go` |
| Trip banners (`trips` category) | `internal/push/notifier_trips.go`, `copy_trips.go` |
| Trip Live Activity | `internal/push/activity_trip_notifier.go`, `activity_trip_fanout.go`, `activity_trip_state.go` |
| Push-to-start envelope | `internal/push/activity_apns.go` |
| Live-side repositories | `internal/store/trip_live_repo.go`, `trip_leg_repo.go`, `trip_activity_token_repo.go`, `live_activity_trip_anchor.go` |
| WebSocket re-mask | `internal/ws/access_revalidator.go`, `client.go`, `hub_location_frames.go` |
| Composition | `cmd/telemetry-server/wiring_trips_live.go` |
