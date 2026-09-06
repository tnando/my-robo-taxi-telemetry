package ws

import "log/slog"

// The role-split fan-out for NON-vehicle_update frames that carry a coordinate
// (MYR-602).
//
// THE GAP THIS CLOSES. Hub.Broadcast is role-BLIND by design, and its own doc
// comment states the condition that makes that safe: "appropriate ONLY for
// messages whose payload contains no role-restricted fields (e.g.,
// drive_started / drive_ended / connectivity in v1)". That parenthesis was true
// when it was written and MYR-602 falsified it. `drive_started` carries
// `startLocation: {latitude, longitude}` — the car's raw position at the moment
// it pulled away — and it is fanned out through the unmasked path to every
// authorized client. A plain `viewer` therefore kept receiving a precise
// coordinate for the owner's car on every drive, straight past the mask that
// had just been narrowed to take exactly that away.
//
// It is the worse half of the leak, not the smaller one: a `vehicle_update`
// stream is a position that decays in seconds, whereas `drive_started` is
// pinned to the ONE moment that reliably locates a person — where they were
// when they set off. Twice a day, that is a home and a workplace.
//
// `drive_ended` RIDES THIS PATH TOO, and for a related but distinct reason. It
// carries no coordinate — it summarises distance, duration and the two speeds —
// but since the narrowing a plain `viewer` gets no drives on any surface (§7.2
// is owner-and-participant; §7.30.7 is what a trip adds), so those four numbers
// are a behavioural record of somebody's driving reaching a person who is not
// entitled to the drive at all. Its redacted frame ZEROES them rather than
// dropping them, because all four are schema-required; see
// redactedDriveEnded.
//
// `connectivity` is unaffected and still rides Broadcast: it says only that the
// car's socket came up or went down, which is availability and is exactly what
// a plain viewer is FOR.
//
// WHY TWO PRE-MARSHALED FRAMES RATHER THAN A MASK PASS. These payloads are
// typed structs, not the `map[string]any` internal/mask projects, and giving
// them a ResourceType would mean either flattening them into maps on the hot
// path or minting a resource whose "field set" is one nested object. The caller
// builds the two shapes it wants — the true one and the redacted one — and this
// function only decides WHO gets which. The decision itself is one call to
// auth.Role.SeesLiveLocation, so the rule lives where the role vocabulary does
// and cannot drift from the mask table.

// BroadcastByLocationAccess fans out to every client authorized for vehicleID,
// choosing between two pre-marshaled frames by the client's ROLE: `located` for
// a role that receives the Speed/GPS group, `redacted` for one that does not.
//
// A redacted frame of nil means WITHHOLD ENTIRELY from those clients — the
// caller's choice between "you get this event with the coordinate blanked" and
// "you do not get this event". drive_started passes a redacted frame, because
// the FACT that a drive began is not itself location (a viewer already sees the
// car's `status` flip to driving) and suppressing it would make the viewer's
// drive list and the owner's disagree about whether anything happened.
//
// The empty Role("") sentinel is treated as NOT seeing live location, i.e. it
// receives the redacted frame — the fail-closed direction. It cannot be
// withheld outright here without diverging from Broadcast's own behaviour for
// such a client, and the redacted frame carries no location to leak.
func (h *Hub) BroadcastByLocationAccess(vehicleID string, located, redacted []byte) {
	if len(located) == 0 {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		if !client.hasVehicle(vehicleID) {
			continue
		}
		frame := located
		if !client.roleFor(vehicleID).SeesLiveLocation() {
			frame = redacted
		}
		if len(frame) == 0 {
			// Withheld for this role by the caller's choice.
			continue
		}
		if dropped := client.enqueue(frame); dropped {
			h.metrics.IncMessagesDropped()
			h.logger.Debug("dropped location-split message for slow client",
				slog.String("user_id", client.userID),
				slog.String("vehicle_id", vehicleID),
			)
		}
	}
}

// noLocationSentinel is the coordinate a redacted frame carries in place of a
// real one: the documented (0, 0) no-fix sentinel from
// vehicle-state-schema.md §2.3 — the same value the mask table's own
// substitution (internal/mask/sentinels.go) writes on the vehicle_update
// surface. Stated here as a named constant so the two surfaces cannot drift
// onto different "nothing known" values.
const noLocationSentinel = 0.0

// redactedStartLocation is the startLocation a viewer's drive_started carries.
// A function rather than a package var so no caller can accidentally hold a
// reference to a shared value it might later mutate.
func redactedStartLocation() startLocation {
	return startLocation{Latitude: noLocationSentinel, Longitude: noLocationSentinel}
}

// redactedDriveEnded is the drive_ended a role without live-location access
// receives: the same frame, with the four summary numbers zeroed.
//
// ZEROED RATHER THAN DROPPED because `distance`, `durationSeconds`, `avgSpeed`
// and `maxSpeed` are all in DriveEndedPayload's `required` list under
// `additionalProperties: false` — removing a key makes the whole document
// undecodable on a strictly-typed client, taking `vehicleId` and `driveId` down
// with the numbers that were the only thing at issue. The same collision the
// vehicle_state sentinels resolve, resolved the same way.
//
// The IDs and the timestamp survive: the event is that a drive ENDED, which a
// viewer already infers from the car's status, and a frame that could not name
// which drive would be strictly less useful without being any more private.
func redactedDriveEnded(full driveEndedPayload) driveEndedPayload {
	full.Distance = 0
	full.DurationSeconds = 0
	full.AvgSpeed = 0
	full.MaxSpeed = 0
	return full
}
