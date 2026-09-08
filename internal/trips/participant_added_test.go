package trips

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// MYR-618: the owner, and only the owner, hears that somebody else widened
// their roster.

// TestNotifyTripParticipantAdded_GoesToTheOwnerAlone is the whole audience rule.
//
// Every other trips notification goes to participants and skips the owner,
// because it announces something the owner did. This one is the inverse and for
// the same reason: it announces something done TO the owner's car's audience by
// somebody else.
func TestNotifyTripParticipantAdded_GoesToTheOwnerAlone(t *testing.T) {
	svc, trips, _, pusher, _, reval := newTestService(t)
	trips.audience["trip-1"] = TripAudience{
		TripID: "trip-1", VehicleID: "veh-1",
		OwnerUserID:        "usr_owner",
		ParticipantUserIDs: []string{"usr_nabil", "usr_joey"},
	}

	err := svc.NotifyTripParticipantAdded(context.Background(), "trip-1", "Nabil", []string{"Joey"})
	if err != nil {
		t.Fatalf("NotifyTripParticipantAdded: %v", err)
	}

	if len(pusher.sent) != 1 {
		t.Fatalf("sent %d pushes, want 1 — got %v", len(pusher.sent), pusher.events())
	}
	sent := pusher.sent[0]
	if sent.Event != push.TripEventParticipantAdded {
		t.Errorf("event = %q, want trip_participant_added", sent.Event)
	}
	if len(sent.UserIDs) != 1 || sent.UserIDs[0] != "usr_owner" {
		t.Fatalf("audience = %v, want [usr_owner] — the participants already know", sent.UserIDs)
	}
	if sent.VehicleID != "veh-1" {
		t.Errorf("vehicleId = %q; the banner names the car and the deep link needs it", sent.VehicleID)
	}
	if sent.ActorName != "Nabil" || len(sent.AddedNames) != 1 || sent.AddedNames[0] != "Joey" {
		t.Errorf("names = (%q, %v), want (Nabil, [Joey]) — passed through, not re-resolved",
			sent.ActorName, sent.AddedNames)
	}

	// NO RE-MASK NUDGE. The access that changed belongs to the person who was
	// ADDED, and NotifyTripAdded has already asked for that sweep; the owner's
	// access to their own car is not a function of the roster.
	svc.DrainRevalidation()
	if reval.count() != 0 {
		t.Errorf("the owner's banner asked for %d re-mask sweeps, want 0", reval.count())
	}
}

// TestNotifyTripParticipantAdded_OwnerlessTripSendsNothing. An audience with no
// owner id has nobody to address, and a push to "" would be a fan-out over an
// empty device list dressed as a delivery.
func TestNotifyTripParticipantAdded_OwnerlessTripSendsNothing(t *testing.T) {
	svc, trips, _, pusher, _, _ := newTestService(t)
	trips.audience["trip-1"] = TripAudience{TripID: "trip-1", VehicleID: "veh-1"}

	if err := svc.NotifyTripParticipantAdded(context.Background(), "trip-1", "Nabil", []string{"Joey"}); err != nil {
		t.Fatalf("NotifyTripParticipantAdded: %v", err)
	}
	if len(pusher.sent) != 0 {
		t.Errorf("sent %d pushes with no owner to address", len(pusher.sent))
	}
}

// TestNotifyTripParticipantAdded_FailedLookupIsNotAnError. The roster change has
// already committed; a banner that could not be addressed must not report a
// failure the caller would have to interpret, and the owner sees the roster the
// next time they open the trip.
func TestNotifyTripParticipantAdded_FailedLookupIsNotAnError(t *testing.T) {
	svc, trips, _, pusher, _, _ := newTestService(t)
	trips.audErr = errors.New("database is down")

	if err := svc.NotifyTripParticipantAdded(context.Background(), "trip-1", "Nabil", []string{"Joey"}); err != nil {
		t.Fatalf("NotifyTripParticipantAdded returned %v, want nil", err)
	}
	if len(pusher.sent) != 0 {
		t.Errorf("sent %d pushes after a failed audience read", len(pusher.sent))
	}
}
