package trips

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// MYR-618's one transition: a PARTICIPANT widened somebody else's roster.
//
// Its own file rather than a seventh function in transitions.go, and not only
// for the 300-line cap. Everything in transitions.go is a WINDOW EDGE — a trip
// opened, a trip closed, somebody was put on one — reached by a sweeper tick,
// an owner's tap, or a clock. This is the first trips notification whose
// subject is an act by a third party, whose audience is the owner alone, and
// which claims no stamp because there is no edge to be idempotent about: it
// announces a change that already committed, once, from the request that made
// it.

// NotifyTripParticipantAdded delivers `trip_participant_added` to the trip's
// OWNER — the one push on this surface whose audience is the owner alone.
//
// THE RECIPIENT IS READ HERE, NOT PASSED. The REST lane holds no owner id: the
// wire never carries one (a trip's participants must not be handed durable
// identifiers for each other, and the owner is named only by `ownerFirstName`),
// so the audience read this package already does for every other event is also
// the only place the owner id exists. A failed read is logged and the banner is
// skipped: the roster change itself has already committed, and the owner sees
// it the next time they open the trip.
//
// NO RE-MASK NUDGE, unlike NotifyTripAdded. The people whose access just
// changed are the ones who were ADDED, and that call has already asked for the
// sweep; the owner's own access to their own car is not a function of the
// roster.
func (s *Service) NotifyTripParticipantAdded(ctx context.Context, tripID, actorName string, addedNames []string) error {
	audience, err := s.trips.TripAudienceFor(ctx, tripID)
	if err != nil {
		s.logger.Warn("trips: trip_participant_added lookup failed; the owner was not notified",
			slog.String("trip_id", tripID),
			slog.String("error", err.Error()))
		return nil
	}
	if audience.OwnerUserID == "" {
		return nil
	}
	s.notify(ctx, push.TripPush{
		TripID:     tripID,
		VehicleID:  audience.VehicleID,
		Event:      push.TripEventParticipantAdded,
		UserIDs:    []string{audience.OwnerUserID},
		ActorName:  actorName,
		AddedNames: addedNames,
	})
	return nil
}
