package push

import "strings"

// Notification copy for the `trips` category (MYR-602).
//
// PAYLOAD POLICY, AND THE ONE DELIBERATE EXTENSION TO IT. copy.go's standing
// rule is that an alert body may name a REQUESTER'S FIRST NAME and a VEHICLE
// NICKNAME, and nothing else about the journey — no place labels, no addresses,
// no coordinates. The two LEG alerts here name a PLACE, and that is a narrow,
// argued carve-out rather than a lapse:
//
//   - The recipient is a trip participant inside an open window. The very same
//     place name is already streaming to their phone on the `destinationName`
//     wire field (their role carries the whole navigation group) and is already
//     rendered on their lock screen by the leg's Live Activity, which fires in
//     the same second as this banner. §7.21.3 permits it there for the rider's
//     own ride on the rider's own device; a trip participant is the same kind
//     of party, chosen explicitly by the owner for exactly this.
//   - Refusing it here would not withhold anything. It would produce a banner
//     saying "the car is going somewhere" beside a card saying where — one
//     surface hedging about a fact the other states.
//
// THE TRIP NAME IS NEVER INTERPOLATED, and that is the line this file holds.
// A trip name is free text a person typed ("DFW → LA with Mom"), it is P1 user
// content, it is the one field in this feature nobody else has vetted, and a
// banner is the surface that renders to whoever is holding an unlocked-enough
// phone. The Live Activity carries it (the card belongs to the trip); the
// banner does not.
//
// Every string below is a constant or is built from exactly two interpolations,
// both of which go through the same enforcement helpers the ride copy uses:
// vehicleLabel for the nickname, truncateLabel for the place.

// tripAlert renders the title/body for one trips event.
//
// vehicleName may be "" (the car has no nickname, or the lookup failed) and
// destination may be "" on the two leg events (a leg cannot start without a
// destination, so this only happens if the name failed to decrypt) — both fall
// back rather than producing an empty sentence.
//
// The second return reports whether this event has copy at all. It is always
// true today and is returned anyway, matching statusAlert's shape, so that a
// future silent event is expressed by the switch rather than by an empty alert
// nobody notices going out.
func tripAlert(event TripEvent, vehicleName, destination string) (alert, bool) {
	car := tripVehicleLabel(vehicleName)
	switch event {
	case TripEventAdded:
		return alert{
			title: "You've been added to a trip",
			body:  car + " will share its location with you for the trip.",
		}, true
	case TripEventStarted:
		return alert{
			title: "Trip started",
			body:  "You can follow " + car + " until the trip ends.",
		}, true
	case TripEventEnded:
		return alert{
			title: "Trip ended",
			body:  car + " is no longer sharing its location.",
		}, true
	case TripEventLegStarted:
		return alert{
			title: car + " is on the move",
			body:  headingTo(destination),
		}, true
	case TripEventLegArrived:
		return alert{
			title: car + " has arrived",
			body:  arrivedAt(destination),
		}, true
	default:
		return alert{}, false
	}
}

// tripVehicleLabel is vehicleLabel with a DIFFERENT fallback, and the
// difference is the whole reason it exists: copy.go's fallback is "Your car",
// which is true of every ride push (the recipient is riding in it or owns it)
// and false of most trip pushes — a participant is watching SOMEBODY ELSE'S
// car. "The car" is the neutral form that reads correctly for a participant and
// is not wrong for the owner, who is also on the leg fan-out.
//
// The trimming and the length cap are shared, so a pathological nickname is
// bounded here exactly as it is everywhere else.
func tripVehicleLabel(name string) string {
	trimmed := truncateRunes(strings.TrimSpace(name), maxNameRunes)
	if trimmed == "" {
		return "The car"
	}
	return trimmed
}

// headingTo renders the leg-start body, naming the place when there is one.
func headingTo(destination string) string {
	if destination == "" {
		return "It has set off."
	}
	return "Heading to " + truncateLabel(destination) + "."
}

// arrivedAt renders the leg-arrival body.
func arrivedAt(destination string) string {
	if destination == "" {
		return "It has reached its destination."
	}
	return "It reached " + truncateLabel(destination) + "."
}
