package trips

import "context"

// REFRESHING A RUNNING CARD, which is the one leg delivery that is not an edge.
//
// Split from detector_decide.go under the 300-line cap, and the seam is the
// receiver: everything left there hangs off Detector and decides what a frame
// MEANS, while this hangs off Service and projects a frame onto a card. A
// future ticker could call it on the same terms without going through the
// detector at all.

// updateLeg refreshes a running card mid-leg.
//
// THE THROTTLE IS THE CALLER'S (vehicleState.dueForCard) rather than this
// function's, because it is per-VIN state and this is a stateless projection.
// What matters is that it exists: Apple throttles high-frequency Activity
// pushes by budget and a car streams up to once per second, so a refresh has to
// earn its push — the arrival minute must have moved, and a floor interval must
// have passed. The card's 3-minute stale-date is what keeps it honest between
// pushes.
//
// It lives on Service rather than Detector because the leg lifecycle belongs
// together, and because a future ticker could call it on the same terms.
func (s *Service) updateLeg(ctx context.Context, leg Leg, f fix, minutes *int) {
	if s.activities == nil || minutes == nil {
		return
	}
	// NO AUDIENCE ARGUMENT (MYR-612). The only thing legContext ever read from
	// it here was the vehicle id, which the LEG already carries — the leg row's
	// `vehicle_id` and the audience's are the same column by construction — so
	// requiring one meant a per-frame audience query for a projection that
	// never used the rest of it.
	tc := s.legContext(ctx, leg, TripAudience{VehicleID: leg.VehicleID}, tripStatusEnroute, &f.at)
	tc.ETAMinutes = minutes
	s.activities.UpdateLeg(ctx, tc)
}

// etaMinutesFrom reads the car's own arrival estimate off a frame.
//
// NIL WHEN THE CAR DOES NOT SAY, never a computed guess. There is no route
// solver in this service, and MYR-194 rules out inventing a number in as many
// words: an absent `eta` renders a card with no time, which is a first-class
// state the client already handles.
func etaMinutesFrom(f fix) *int {
	if f.minutesToGo == nil {
		return nil
	}
	m := int(*f.minutesToGo + 0.5)
	if m < 0 {
		return nil
	}
	return &m
}
