package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/trips"
)

// tripNotifierAdapter joins the two halves of MYR-602 at the composition root:
// the REST handlers in internal/telemetry, which announce the three events a
// REQUEST causes, and internal/trips, which owns every fan-out and the closing
// edge's machinery.
//
// IT EXISTS BECAUSE THE TWO PACKAGES NAME THE SAME EVENTS DIFFERENTLY, and
// neither should be made to speak the other's vocabulary. The handler side is
// written in terms of the trip it just wrote — it holds the whole TripData and
// the exact set of people it just added — while the live side is written in
// terms of a trip ID it will read for itself, because the sweeper reaches the
// same edges with nothing in hand but an id it claimed. Renaming either would
// make one lane's seam wrong for its own caller; converting here costs three
// one-line methods and keeps both honest.
//
// THE ERRORS STOP HERE, and that is the seam's contract rather than laziness.
// telemetry.TripNotifier returns nothing on purpose: every call happens AFTER
// the transaction has committed, so there is no error the handler could act on
// — the trip exists, and a failed announcement must not be able to unmake it.
// The live side does return errors, because the sweeper retries on its next
// pass, so this adapter is where the two policies meet: log it and move on.
type tripNotifierAdapter struct {
	svc    *trips.Service
	logger *slog.Logger
}

// Compile-time proof that the live service satisfies the handler lane's seam
// through this adapter. Stated here so a signature drift on either side fails
// at build rather than at boot.
var _ telemetry.TripNotifier = (*tripNotifierAdapter)(nil)

// TripAdded announces `trip_added` to the people the request just put on the
// trip. The ids are passed through rather than re-read: on a create nobody else
// can see the participants yet, and on a PATCH only the new arrivals are news.
func (a *tripNotifierAdapter) TripAdded(ctx context.Context, trip telemetry.TripData, userIDs []string) {
	if err := a.svc.NotifyTripAdded(ctx, trip.ID, userIDs); err != nil {
		a.log(ctx, "trip_added", trip.ID, err)
	}
}

// TripStarted announces a window that opened under its own create — an owner
// making a trip that starts "now", which is the common shape for a road trip
// already underway. Every other opening is the sweeper's, and the two cannot
// double-announce because both go through the same `started_notified_at` claim.
func (a *tripNotifierAdapter) TripStarted(ctx context.Context, trip telemetry.TripData, _ []string) {
	if err := a.svc.NotifyTripStarted(ctx, trip.ID); err != nil {
		a.log(ctx, "trip_started", trip.ID, err)
	}
}

// TripEnded is the owner's early end, and it is SETTLEMENT rather than an
// announcement: NotifyTripEnded is trips.Service.SettleTrip under the name the
// handler lane knows it by, so this one call claims the closing stamp, ends
// every open leg and its Live Activity BEFORE the banner goes out, fans out
// `trip_ended`, and nudges the WebSocket re-mask.
//
// That ordering is why the end handler does not send a push of its own: a card
// still saying "heading to the Grand Canyon" is a lie on a lock screen, while a
// missing banner is only a silence — and only the live package can end a card.
func (a *tripNotifierAdapter) TripEnded(ctx context.Context, trip telemetry.TripData, _ []string) {
	if err := a.svc.NotifyTripEnded(ctx, trip.ID); err != nil {
		a.log(ctx, "trip_ended", trip.ID, err)
	}
}

// TripDeleted settles a trip the owner is about to delete (MYR-607): it ends
// every open leg and its Live Activities, then fans out `trip_ended` carrying
// `deleted: true`.
//
// SAME SHAPE AS TripEnded AND FOR THE SAME REASON — the settlement is the work,
// the banner is a side effect of it — with the one difference that this one is
// the LAST moment the trip can be read at all. The handler calls it before the
// delete; nothing after the delete could end a card, because nothing after the
// delete can name the device holding it.
func (a *tripNotifierAdapter) TripDeleted(ctx context.Context, trip telemetry.TripData, _ []string) {
	if err := a.svc.NotifyTripDeleted(ctx, trip.ID); err != nil {
		a.log(ctx, "trip_deleted", trip.ID, err)
	}
}

// log records a failed announcement at WARN. Not ERROR: the state change it was
// about has already committed and is correct, and the sweeper reaches the same
// edge again on its next pass for both boundary events.
//
// The trip NAME is never logged — it is P1 user content sealed at rest
// (data-classification.md §1.25) — so the line carries the opaque id only.
func (a *tripNotifierAdapter) log(ctx context.Context, event, tripID string, err error) {
	a.logger.WarnContext(ctx, "trips: notification failed; the trip itself is unaffected",
		slog.String("event", event),
		slog.String("trip_id", tripID),
		slog.String("error", err.Error()))
}

// tripNotifier builds the handler-lane seam over the live runtime, or returns
// nil when there is no runtime to build it over.
//
// RETURNING A TYPED NIL WOULD BE THE BUG HERE. `tripsLive.Service` is nil
// whenever TRIPS_ENABLED is false, and wrapping a nil service in an adapter
// would produce a NON-nil telemetry.TripNotifier whose every method
// dereferences nothing — a panic after a commit that already succeeded, on the
// one path where the feature is supposed to be switched off. Returning an
// untyped nil is what makes setupTripEndpoints' `notifier != nil` check mean
// what it reads as.
func tripNotifier(runtime *tripsLive, logger *slog.Logger) telemetry.TripNotifier {
	if runtime == nil || runtime.Service == nil {
		return nil
	}
	return &tripNotifierAdapter{svc: runtime.Service, logger: logger.With(slog.String("component", "trips"))}
}
