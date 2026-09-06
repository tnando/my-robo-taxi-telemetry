// Package trips owns the LIVE half of MYR-602: the clock that opens and closes
// a trip's window, and the telemetry that turns the car's driving into legs.
//
// # What a trip is, and what this package is not
//
// A trip is a (vehicle, window, participant set) tuple created by the vehicle's
// owner. Everything a REST caller can do with one — create, list, read, patch,
// end, leave, read its drives — lives elsewhere. This package never serves a
// request. It has exactly two jobs, and both of them happen with nobody
// watching:
//
//	the SWEEPER   every 60s, moves trips across their two window edges
//	the DETECTOR  on every telemetry frame, opens and closes driving legs
//
// # Why the sweeper is a sweeper
//
// A share is revoked by somebody clicking something; a window opens and closes
// on the CLOCK. Nothing is written at `starts_at`, nobody calls anything at
// `ends_at`, and a millisecond past either instant the only thing that has
// changed is what `NOW()` returns inside the access query. There is no mutation
// to hang an event off, so the pass IS the mechanism and its interval is the
// whole latency budget — which is also why internal/ws's AccessRevalidator runs
// at the same 60 seconds and is nudged by every transition this package makes.
//
// Each edge is claimed with a single `UPDATE … RETURNING` against a stamped
// column, so two processes during a rolling deploy cannot both announce the
// same trip. The stamp is written BEFORE the fan-out, which is the opposite of
// the Live Activity marks and is argued in store.queryClaimTripsToStart: a
// missed "your trip started" is recoverable by opening the app, and a repeating
// one is not recoverable at all.
//
// # Why the leg detector reads raw telemetry
//
// A LEG is one journey inside a window: the car sets off for a named place,
// drives, and arrives or parks. Legs govern ONLY the Live Activity and the two
// leg pushes — ACCESS IS PURELY THE WINDOW (client ruling, 2026-09-05), so a
// parked car inside an open window streams its position to its participants
// exactly as it does to its owner, leg or no leg.
//
// THERE IS NO "DRIVING WITH A DESTINATION" EVENT IN THIS SERVICE. The obvious
// seam, `drive.started` from internal/drives, is a SHIFT-STATE machine: it
// fires when the car moves out of P, it carries a VIN, a drive id and a
// coordinate, and it knows nothing whatever about navigation. internal/arrival
// is the other candidate and is RIDE-SCOPED to its bones — its candidate set is
// built from `go_ride_requests` rows with a dispatched pickup, and its verdict
// writes a ride status.
//
// Two ways to close that gap were available:
//
//  1. EXTEND DriveStartedEvent with the destination. Rejected. The drive
//     detector would have to start consuming navigation fields to populate it,
//     which makes a state machine about GEAR depend on a group it has no reason
//     to know; every existing consumer of that event would carry a field only
//     this package reads; and the timing is wrong in a way that cannot be fixed
//     from there — a driver commonly sets the destination on the dash AFTER
//     pulling out, so a destination sampled at the drive-start instant is
//     empty on a large fraction of real legs.
//
//  2. A PER-VIN CACHE off TopicVehicleTelemetry, which is what this package
//     does. It watches the frames it already receives, remembers the last
//     destination and the last motion verdict per VIN, and opens a leg on the
//     TRANSITION into "driving, with a destination" from either direction —
//     the car started moving with a route already set, or the car was already
//     moving and a route appeared. The second case is the common one and is
//     precisely what option 1 could not express.
//
// The cost is that this package subscribes to the busiest topic in the service.
// It is bounded the same way internal/arrival's is: the per-frame path is two
// map lookups and, for a car in an open window, a comparison — the trip
// candidate set is cached (see legCandidates) and only one frame per TTL pays
// for a query.
//
// # Arrival evidence, and the one place this differs from internal/arrival
//
// internal/arrival refuses to treat the car's own `milesToArrival` as evidence,
// and says why in as many words: the dash's target and the RIDE's target are
// different facts, and MYR-527 is the standing proof (a rider spent three hours
// on a trip whose car was still navigating to the pickup).
//
// ON A TRIP LEG THAT ARGUMENT INVERTS, and the inversion is exact rather than
// convenient: a leg is DEFINED as "the car is driving to the place the dash
// says", so the dash's target IS the leg's target, by construction. The
// distance the car reports to its own destination is therefore the most direct
// evidence available, and it is used — together with the same stillness rule
// (the car must actually be stopped) and the same 80 m / 20 s dwell thresholds,
// so a leg cannot "arrive" at a red light 60 m short of the destination.
//
// The destination COORDINATE is used when the car streams one
// (`destinationLocation`), and the reported distance is the fallback when it
// does not. Either way the verdict needs stillness, and a leg that ends without
// it — the car parked somewhere else, the route was cleared, the window closed
// underneath it — is `completed`, not `arrived`: no `trip_leg_arrived` push,
// and a final card that says the drive ended rather than that it arrived.
package trips
