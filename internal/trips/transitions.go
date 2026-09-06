package trips

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// The two window transitions, and the TripNotifier surface the REST lane calls.
//
// ONE FUNCTION PER EDGE, whoever noticed it. The sweeper's ticker, the owner's
// "End trip" tap and a window that elapsed while the server was down all arrive
// here, and the claim inside each is what makes "at most once" hold across all
// three at the same instant.

// TripNotifier is the surface the trips REST handlers call. Declared here
// because this package is what implements it; the handler lane holds it as an
// interface of its own so the two can land independently.
//
// Deliberately TINY. Everything these three need — who is on the trip, what the
// car is called, whether the window is open — this package can read for itself,
// and a wider signature would make every handler responsible for assembling an
// audience it has no other reason to know.
type TripNotifier interface {
	// NotifyTripAdded tells the named people they were put on a trip. The
	// caller passes the ids because it JUST created them, and a read-back would
	// race its own transaction: on create the participants do not exist for
	// anybody else yet, and on a PATCH only the NEWLY added ones are news.
	NotifyTripAdded(ctx context.Context, tripID string, userIDs []string) error
	// NotifyTripStarted announces a window that opened. Idempotent through the
	// same stamp the sweeper claims, so a create-with-a-past-start racing the
	// sweeper's own pass announces once.
	NotifyTripStarted(ctx context.Context, tripID string) error
	// NotifyTripEnded announces a window that closed, ends every open leg and
	// its cards, and re-masks the sockets. It is SettleTrip under the name the
	// handler lane knows it by.
	NotifyTripEnded(ctx context.Context, tripID string) error
}

// Compile-time proof that the sweeper's own service satisfies the surface the
// REST lane calls. Stated rather than left to the wiring, so a signature drift
// between the two lanes fails here rather than at composition.
var _ TripNotifier = (*Service)(nil)

// NotifyTripAdded delivers `trip_added` to the given people.
//
// The ids come from the caller and the trip is read only for the vehicle, which
// the payload carries so the app can open the right car without a second call.
// A read that fails is logged and the push is skipped rather than sent with an
// empty vehicleId — a deep link into a car the client cannot name is worse than
// a missing banner, and the person will see the trip the next time they open
// the app either way.
func (s *Service) NotifyTripAdded(ctx context.Context, tripID string, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	audience, err := s.trips.TripAudienceFor(ctx, tripID)
	if err != nil {
		s.logger.Warn("trips: trip_added lookup failed; nobody notified",
			slog.String("trip_id", tripID),
			slog.String("error", err.Error()))
		return nil
	}
	s.notify(ctx, push.TripPush{
		TripID:    tripID,
		VehicleID: audience.VehicleID,
		Event:     push.TripEventAdded,
		UserIDs:   userIDs,
	})
	// A person added to a trip whose window is ALREADY OPEN needs their socket
	// re-masked now, not on the next tick: they were a plain viewer a moment
	// ago and the app they are about to open expects a live map.
	s.nudgeRevalidation(ctx, "trip_added", tripID)
	return nil
}

// NotifyTripStarted opens a window: claims the start stamp, announces it, and
// nudges the re-mask.
//
// The claim is what makes this idempotent across the three callers. A create
// whose `startsAt` is already in the past reaches here from the handler at the
// same moment the sweeper's pass may be claiming the same row; exactly one of
// them wins the UPDATE and exactly one fan-out happens.
func (s *Service) NotifyTripStarted(ctx context.Context, tripID string) error {
	return s.startTrip(ctx, tripID)
}

// NotifyTripEnded is SettleTrip under the name the REST lane calls it by.
func (s *Service) NotifyTripEnded(ctx context.Context, tripID string) error {
	return s.SettleTrip(ctx, tripID)
}

// startTrip is the opening edge. Claimed by the caller when the caller is the
// sweeper (it claims in bulk); claimed here when it is a handler.
func (s *Service) startTrip(ctx context.Context, tripID string) error {
	claimed, err := s.trips.ClaimTripStartNow(ctx, tripID)
	if err != nil {
		return fmt.Errorf("trips.startTrip(trip=%s): %w", tripID, err)
	}
	if !claimed {
		return nil
	}
	s.announceStart(ctx, tripID)
	return nil
}

// announceStart fans out `trip_started` and re-masks. Split from the claim so
// the SWEEPER, which claims in bulk, can call it directly for each id it won.
func (s *Service) announceStart(ctx context.Context, tripID string) {
	audience, err := s.trips.TripAudienceFor(ctx, tripID)
	if err != nil {
		s.logger.Warn("trips: trip_started lookup failed; nobody notified",
			slog.String("trip_id", tripID),
			slog.String("error", err.Error()))
		// The re-mask still runs. The push is a courtesy; the ACCESS is the
		// feature, and it must not be held hostage to a failed read of who to
		// congratulate.
		s.nudgeRevalidation(ctx, "trip_started", tripID)
		return
	}
	s.notify(ctx, push.TripPush{
		TripID:    tripID,
		VehicleID: audience.VehicleID,
		Event:     push.TripEventStarted,
		// PARTICIPANTS ONLY. The owner set the window themselves; telling them
		// their own trip started is the class of noise the self-ride
		// suppression in internal/push exists to remove.
		UserIDs: audience.ParticipantUserIDs,
	})
	s.nudgeRevalidation(ctx, "trip_started", tripID)
	s.logger.Info("trip window opened",
		slog.String("trip_id", tripID),
		slog.String("vehicle_id", audience.VehicleID),
		slog.Int("participants", len(audience.ParticipantUserIDs)),
	)
}

// SettleTrip closes a trip: claims the end stamp, ends every open leg and its
// cards, announces `trip_ended`, and nudges the re-mask.
//
// THE SEAM THE OWNER'S EARLY END CALLS. The handler writes `ended_at` and then
// calls this, so the participants hear about it in the same second rather than
// up to a sweep later — and because the claim is the same one the sweeper uses,
// an early end racing the sweeper's pass produces exactly one fan-out.
//
// ORDERING IS NORMATIVE: the legs are ended BEFORE the trip announcement. Both
// send pushes, and the leg's is the more urgent of the two — it is what takes a
// live card off a lock screen — so a failure in the trip fan-out must not be
// able to leave a card running. Doing it in the other order would also produce
// the wrong sentence on the phone: "the trip has ended" arriving while a card
// still says the car is driving to the Grand Canyon.
//
// Idempotent: a second call claims nothing and returns nil, which is what makes
// a double tap on End trip harmless.
func (s *Service) SettleTrip(ctx context.Context, tripID string) error {
	claimed, err := s.trips.ClaimTripEndNow(ctx, tripID)
	if err != nil {
		return fmt.Errorf("trips.SettleTrip(trip=%s): %w", tripID, err)
	}
	if !claimed {
		return nil
	}
	s.settleClaimed(ctx, tripID)
	return nil
}

// settleClaimed does the work of a closing edge whose stamp is already claimed.
// The SWEEPER claims in bulk and calls this directly.
func (s *Service) settleClaimed(ctx context.Context, tripID string) {
	audience, audErr := s.trips.TripAudienceFor(ctx, tripID)

	// Legs first — see SettleTrip's ordering note. A leg still open when its
	// window closes ended without arrival evidence BY DEFINITION: the car was
	// still driving somewhere when the trip ran out of time, so its card
	// carries `completed` and no `trip_leg_arrived` push fires.
	s.endOpenLegs(ctx, tripID, audience)

	if audErr != nil {
		s.logger.Warn("trips: trip_ended lookup failed; nobody notified",
			slog.String("trip_id", tripID),
			slog.String("error", audErr.Error()))
	} else {
		s.notify(ctx, push.TripPush{
			TripID:    tripID,
			VehicleID: audience.VehicleID,
			Event:     push.TripEventEnded,
			// Participants only, same reasoning as the start: the owner either
			// set this end themselves or set the window it just reached.
			UserIDs: audience.ParticipantUserIDs,
		})
	}

	// LAST, and last on purpose: this is what actually revokes the live
	// location, and running it before the pushes would take a participant's map
	// dark a beat before their phone told them why.
	s.nudgeRevalidation(ctx, "trip_ended", tripID)
	s.logger.Info("trip window closed", slog.String("trip_id", tripID))
}

// endOpenLegs closes whatever legs a trip still has open, with no arrival
// evidence.
//
// The partial unique index permits at most one, and this reads a LIST anyway:
// a cleanup path must close what it FINDS rather than assume the invariant it
// exists to clean up after.
func (s *Service) endOpenLegs(ctx context.Context, tripID string, audience TripAudience) {
	legs, err := s.legs.OpenLegsForTrip(ctx, tripID)
	if err != nil {
		s.logger.Error("trips: open-leg lookup failed; a card may be stranded",
			slog.String("trip_id", tripID),
			slog.String("error", err.Error()))
		return
	}
	for _, leg := range legs {
		s.closeLeg(ctx, leg, audience, false)
	}
}
