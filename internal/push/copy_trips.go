package push

import (
	"strconv"
	"strings"
)

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
// IT TAKES THE WHOLE PUSH rather than the three or five fields it happens to
// read. The events differ from each other in which fields they use — the leg
// pair wants a destination, MYR-618's owner banner wants two names, the
// lifecycle three want neither — and a positional signature that grew a
// parameter per event is how a leg destination ends up in the actor slot. The
// vehicle name stays a separate argument because it is not on the push: it is
// resolved by the notifier against the vehicle id.
//
// vehicleName may be "" (the car has no nickname, or the lookup failed),
// destination may be "" on the two leg events (a leg cannot start without one,
// so this means the name failed to decrypt), and the MYR-618 names may be empty
// — all of them fall back rather than producing an empty sentence.
//
// The second return reports whether this event has copy at all. It is always
// true today and is returned anyway, matching statusAlert's shape, so that a
// future silent event is expressed by the switch rather than by an empty alert
// nobody notices going out.
func tripAlert(p TripPush, vehicleName string) (alert, bool) {
	car := tripVehicleLabel(vehicleName)
	destination := p.DestinationName
	switch p.Event {
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
	case TripEventParticipantAdded:
		return participantAddedAlert(car, p.ActorName, p.AddedNames), true
	default:
		return alert{}, false
	}
}

// participantAddedAlert is MYR-618's owner banner: somebody who is on the
// owner's trip put somebody else on it too.
//
// ⚠ THE TRIP NAME IS NOT IN IT, and that is this file's standing line rather
// than an oversight about the client's phrasing. The ask was "{Participant}
// added {Person} to {Trip}"; a trip name is free text a person typed, routinely
// naming where they are going ("DFW → LA with Mom"), it is P1 user content
// sealed at rest, and a banner renders to whoever is holding a phone that is
// unlocked enough. The CAR is what the owner needs to disambiguate — they may
// have two — and the car's nickname is already permitted here. The trip's own
// name is one tap away, on the sheet the deep link opens.
//
// The names are first names off the trip roster, the same class of value the
// ride copy already interpolates for a requester, and both fall back: an
// unresolvable actor is "Someone", an unresolvable addition is "someone".
func participantAddedAlert(car, actorName string, addedNames []string) alert {
	actor := truncateLabel(strings.TrimSpace(actorName))
	if actor == "" {
		actor = "Someone"
	}
	return alert{
		title: actor + " added " + addedPeopleLabel(addedNames) + " to your trip",
		body:  "They can follow " + car + " until the trip ends.",
	}
}

// addedPeopleLabel names one addition, or counts several.
//
// TWO PEOPLE ARE COUNTED RATHER THAN LISTED, deliberately. A banner is one line
// on a lock screen and a comma-joined list of names is what pushes the sentence
// past it; the count is legible at any length and the sheet behind the deep
// link has the actual roster. One name is the overwhelmingly common case and is
// worth spelling out.
//
// ⚠ THE COUNT IS OVER THE PEOPLE ADDED, NOT OVER THE NAMES THAT RESOLVED, and
// the review round moved it: it counted the trimmed slice, so an add of three
// people one of whom has not been through the naming prompt announced
// "2 people" — a banner that under-reports how many people just gained live
// access to the owner's car, which is the one number this push exists to carry.
// Names can be absent (the MYR-583 confirmation gate is why, and it is common),
// the COUNT never is.
//
// The single-addition arm therefore falls back rather than borrowing the count:
// "someone" for one unresolved name, because "1 people" is not a sentence and
// "1 person" would be a stranger way of saying the same thing.
func addedPeopleLabel(names []string) string {
	if len(names) == 0 {
		// The caller passed none. Only reachable if the fan-out ran with an
		// empty diff, which the handler guards against; "someone" is still a
		// true sentence and a count would be a lie.
		return "someone"
	}
	if len(names) == 1 {
		if only := strings.TrimSpace(names[0]); only != "" {
			return truncateLabel(only)
		}
		return "someone"
	}
	return strconv.Itoa(len(names)) + " people"
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
