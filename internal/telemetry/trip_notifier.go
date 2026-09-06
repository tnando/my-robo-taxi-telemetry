package telemetry

import "context"

// THE PUSH SEAM for the MYR-602 trips surface.
//
// Three of the five `trips` push events are caused by a REST call and are
// therefore this package's to emit: `trip_added` when somebody is put on a
// trip, `trip_started` when a create opens a window that is ALREADY open, and
// `trip_ended` when the owner ends one early. The other two — `trip_leg_started`
// and `trip_leg_arrived` — are caused by TELEMETRY, are emitted by the leg
// detector, and appear nowhere in this file.
//
// WHY AN INTERFACE RATHER THAN A DIRECT CALL INTO internal/push. Two reasons,
// and the second is why the seam is here at all rather than being added later:
//
//   - The dependency rule. internal/telemetry does not import internal/push;
//     the composition root wires the two together, as it does for every other
//     notifier on this surface.
//   - The `trips` push CATEGORY — its prefs toggle, its payload shape, its
//     delivery flags, its deep link — is being built by a SIBLING LANE against
//     the same base. Declaring the seam and calling it lets both halves land
//     independently, and the day the category exists the only change here is
//     one wiring line.
//
// NIL IS A NO-OP, NOT A FAILURE, and that is the load-bearing property: a
// deployment with no notifier wired creates trips that work perfectly and tell
// nobody. A push is an announcement about a state change, never the state
// change itself — so a notifier that is absent, slow or broken must not be able
// to fail a create.

// TripNotifier delivers the three REST-caused `trips` push events.
//
// IMPLEMENTATIONS MUST NOT BLOCK THE REQUEST. Every method is called after the
// database transaction has committed and its result is discarded — there is no
// error return, deliberately, because there is no error the handler could act
// on. A push that failed to send must not undo a trip that exists.
//
// The (trip, recipient) fan-out is the implementation's job: it holds the push
// prefs, the device registry and the per-category toggle, and this package
// holds none of those.
type TripNotifier interface {
	// TripAdded announces that these people are now on this trip.
	//
	// userIDs is the set that was JUST added — on create, everybody; on patch,
	// only the new arrivals. Sending to the whole roster on every patch would
	// re-notify people who were already there, which reads as the trip having
	// been created twice.
	TripAdded(ctx context.Context, trip TripData, userIDs []string)

	// TripStarted announces that a window has OPENED.
	//
	// Called from the REST layer only for the create that lands inside its own
	// window — an owner who makes a trip starting "now", which is the common
	// case for a road trip already underway. Every other transition from
	// scheduled to active happens at an instant no request is present for, and
	// is the trip sweeper's to send. The sweeper's `started_notified_at` stamp
	// is what stops the two sending it twice.
	TripStarted(ctx context.Context, trip TripData, userIDs []string)

	// TripEnded announces that a window has CLOSED EARLY, at the owner's
	// request. The natural expiry is likewise the sweeper's.
	TripEnded(ctx context.Context, trip TripData, userIDs []string)
}

// noopTripNotifier is what a handler uses when no notifier is wired. Named and
// typed rather than a nil check at each of the three call sites, so a fourth
// event added later cannot forget the check.
type noopTripNotifier struct{}

func (noopTripNotifier) TripAdded(context.Context, TripData, []string)   {}
func (noopTripNotifier) TripStarted(context.Context, TripData, []string) {}
func (noopTripNotifier) TripEnded(context.Context, TripData, []string)   {}

// participantUserIDs is the fan-out list for a trip: every live participant.
//
// THE OWNER IS NOT IN IT. All three events announce something the owner just
// did, and a phone that buzzes to tell its owner about their own tap is the
// most common way a notification category gets turned off. The owner IS
// included in the per-leg Live Activity, which is a different mechanism
// answering a different question, and the sibling lane owns that.
func participantUserIDs(trip TripData) []string {
	out := make([]string, 0, len(trip.Participants))
	for _, p := range trip.Participants {
		out = append(out, p.UserID)
	}
	return out
}

// newParticipantUserIDs returns the participants of `after` that were not in
// `before` — the people a PATCH actually added.
func newParticipantUserIDs(before, after TripData) []string {
	had := make(map[string]bool, len(before.Participants))
	for _, p := range before.Participants {
		had[p.UserID] = true
	}
	out := make([]string, 0, 2)
	for _, p := range after.Participants {
		if !had[p.UserID] {
			out = append(out, p.UserID)
		}
	}
	return out
}
