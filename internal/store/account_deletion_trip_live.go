package store

import "context"

// Account deletion, step 8g (MYR-602): the two LIVE trip tables that name a
// person who is not the trip's owner.
//
// WHY THESE TWO NEED A STEP AT ALL, when both cascade. go_trip_activity_tokens
// and the leg-anchored rows of go_live_activities both hang off go_trips
// through ON DELETE CASCADE, so deleting a trip takes them with it. That covers
// the OWNER completely: their trips go, and everything under them goes.
//
// It covers a PARTICIPANT not at all. A participant's push-to-start token and
// their running leg Activity live under SOMEBODY ELSE'S trip, which the
// deletion does not touch and must not — the road trip is still happening and
// the other people on it are still on it. Without this step, a deleted
// account's rows sit under a live trip and are read by the leg detector on the
// next leg: the server would push-to-start a Live Activity onto the phone of an
// account that no longer exists, and keep doing it for the rest of the window.
//
// THE FAILURE IS A DELIVERY, NOT A LEAK, and that is why this is P0 hygiene in
// the 8-family rather than an erasure obligation like 8c. Neither row holds
// anything about the person beyond an opaque cuid and a device capability; what
// makes them worth removing is that they are ADDRESSED, and an address that
// outlives its account is a notification nobody can turn off.
//
// The core lane owns the trip-side deletion (the person's own trips, and their
// participant rows). This file is deliberately scoped to the two tables the
// live half owns, so the two steps can land independently and neither has to
// know the other's row counts.

// queryDeleteTripActivityTokensForUser removes every push-to-start
// registration this person holds, on any trip — their own (which the trip
// cascade would also reach) and other people's (which nothing else would).
//
// P1 CAPABILITY going out of existence: the row holds an APNs push-to-start
// token, which together with the signing key can raise a Live Activity on that
// phone. It is deleted rather than tombstoned for the same reason
// go_push_devices is: the registry's whole value is that it is current, a dead
// token has no evidentiary use, and Apple's own 410 path already deletes by
// value.
//
// Deleting zero rows on a re-run is a clean no-op, and zero is the ordinary
// result: only somebody who opened a trip screen on an iPhone ever has one.
// #nosec G101 -- column/predicate SQL over the push-to-start registry, not a
// credential literal (gosec greps the identifier 'token' in a string).
const queryDeleteTripActivityTokensForUser = `
DELETE FROM go_trip_activity_tokens WHERE user_id = $1`

// queryDeleteTripLegActivitiesForUser removes this person's LEG-anchored Live
// Activity registrations.
//
// SCOPED TO `trip_leg_id IS NOT NULL`, and the predicate is load-bearing rather
// than decorative. Without it this statement would also delete the person's
// RIDE Activities — rows that belong to a different lifecycle, whose end push
// the ride teardown is responsible for sending BEFORE the row goes (see
// ActivityNotifier.EndForVehicleTeardown and the MYR-258 argument about a
// cascade stranding the card rather than the row). Deleting them here would
// take the sender's only address for a card that is still on a lock screen.
//
// The leg rows carry the same hazard in principle, and it is bounded in
// practice by the leg's own life: a leg ends when the car parks or the window
// closes, both of which end the Activity through the leg path. A deleted
// account whose card is mid-leg keeps it until ActivityKit's own staleness
// ceiling retires it — the same outcome the ride path accepts for a rider who
// deletes their account mid-ride, and the alternative (pushing an `end` to a
// phone whose account we are erasing) is worse on every axis.
const queryDeleteTripLegActivitiesForUser = `
DELETE FROM go_live_activities
WHERE user_id = $1 AND trip_leg_id IS NOT NULL`

// DeleteTripActivityTokens removes the account's push-to-start registrations
// (MYR-602, data-lifecycle.md §3.1 step 8g). Returns the number of rows
// deleted. Idempotent.
//
// UNCONSTRAINED IN POSITION, unlike 8e and 8f: nothing in the per-vehicle
// teardown writes one of these rows, so there is no ordering hazard against
// step 3. It must run before the identity delete, like every destructive step.
func (a *AccountDeleter) DeleteTripActivityTokens(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteTripActivityTokens", queryDeleteTripActivityTokensForUser, userID)
}

// DeleteTripLegActivities removes the account's leg-anchored Live Activity
// registrations (MYR-602, step 8g). Returns the number of rows deleted.
// Idempotent, and deliberately leaves the account's RIDE Activities alone — see
// queryDeleteTripLegActivitiesForUser.
func (a *AccountDeleter) DeleteTripLegActivities(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteTripLegActivities", queryDeleteTripLegActivitiesForUser, userID)
}
