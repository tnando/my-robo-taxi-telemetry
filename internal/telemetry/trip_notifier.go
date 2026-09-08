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
//     delivery flags, its deep link — lives in internal/push and is driven by
//     internal/trips, which also owns the sweeper and the leg detector.
//     Declaring the seam and calling it let the two halves land independently;
//     it is satisfied at composition by cmd/telemetry-server's
//     tripNotifierAdapter, which is also where the live side's errors stop.
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

	// TripDeleted settles a trip the owner is about to DELETE (MYR-607,
	// §7.30.10): it ends every open leg and its Live Activities, then announces
	// `trip_ended` carrying `deleted: true`.
	//
	// ⚠ IT IS CALLED BEFORE THE ROWS ARE GONE, and that ordering is normative
	// rather than incidental. Everything it does reads the trip: who is on it,
	// which legs are open, which device holds which card. After the delete
	// there is nothing left in the database that could name any of them, so a
	// settlement that ran afterwards would end no card and tell nobody, and
	// every participant's lock screen would keep a live Activity for a trip
	// that no longer exists until ActivityKit's own staleness ceiling retired
	// it.
	//
	// It is the FOURTH REST-caused event and still not a fourth push event:
	// `deleted` rides `trip_ended` rather than becoming a sixth member of the
	// `trips` category — see push.TripPush.Deleted for why an unknown `event`
	// would be worse than a flag an old build ignores.
	TripDeleted(ctx context.Context, trip TripData, userIDs []string)

	// TripParticipantAdded tells the trip's OWNER that somebody else widened
	// their roster (MYR-618).
	//
	// THE ONLY EVENT ON THIS SURFACE WHOSE AUDIENCE IS THE OWNER ALONE, and the
	// asymmetry is the reason it exists: every other trips push announces
	// something the owner did, so sending it to them would be telling a person
	// about their own tap. This one announces something done TO their car's
	// audience by somebody else, which is the one thing on the platform that
	// changes who can watch an owner's vehicle without the owner acting.
	//
	// THE NAMES ARE PASSED RATHER THAN LOOKED UP, and they are the ROSTER's
	// names — the confirmed first name, else the owner's own label for the
	// grant. The handler has just read them back; resolving them again in the
	// live half would be a second ladder that could disagree with the trip
	// sheet the owner opens from the banner. `actorName` may be empty and
	// `addedNames` may be short if a name failed to resolve; the copy layer
	// falls back rather than rendering an empty sentence.
	//
	// The recipient is resolved by the implementation, which reads the trip's
	// audience for itself — this package holds no owner id (the wire never
	// carries one) and must not start.
	TripParticipantAdded(ctx context.Context, trip TripData, actorName string, addedNames []string)

	// ActivityTokenRegistered raises the current leg's Live Activity on the
	// phone that has just registered a push-to-start token (MYR-612).
	//
	// IT TAKES IDS RATHER THAN A TripData, alone among the six, because it is
	// not an announcement about a trip — it is a repair addressed to ONE
	// device, and the live side reads everything else it needs for itself.
	//
	// ⚠ IT IS WHY ANYBODY GETS A CARD AT ALL ON THE FIRST LEG. A leg's Activity
	// is push-to-start and the fan-out runs once, when the leg opens, over
	// whatever tokens exist then — while registering is what a phone does when
	// the leg-start push WAKES it, necessarily afterwards. On 2026-09-08 the
	// only participant registered three seconds late and the trip ran all
	// evening with no card for anybody.
	//
	// Called on every registration, including the overwhelming majority with no
	// leg open: the live side's per-(device, leg) claim is the idempotency.
	ActivityTokenRegistered(ctx context.Context, tripID, userID string)
}

// noopTripNotifier is what a handler uses when no notifier is wired. Named and
// typed rather than a nil check at each of the three call sites, so a fourth
// event added later cannot forget the check.
type noopTripNotifier struct{}

func (noopTripNotifier) TripAdded(context.Context, TripData, []string)                    {}
func (noopTripNotifier) TripStarted(context.Context, TripData, []string)                  {}
func (noopTripNotifier) TripEnded(context.Context, TripData, []string)                    {}
func (noopTripNotifier) TripDeleted(context.Context, TripData, []string)                  {}
func (noopTripNotifier) TripParticipantAdded(context.Context, TripData, string, []string) {}
func (noopTripNotifier) ActivityTokenRegistered(context.Context, string, string)          {}

// participantUserIDs is the fan-out list for a trip: every live participant.
//
// THE OWNER IS NOT IN IT. All three events announce something the owner just
// did, and a phone that buzzes to tell its owner about their own tap is the
// most common way a notification category gets turned off. The owner IS
// included in the per-leg Live Activity, which is a different mechanism
// answering a different question, and internal/trips owns that.
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
