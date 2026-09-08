package telemetry

import (
	"time"
)

// EVERY WIRE PROJECTION FOR §7.30 — what this surface SENDS. The request bodies
// it receives, and their validation, are in trip_request.go: the two directions
// are separate concerns and the file was over the 300-line cap holding both.
//
// ONE PROJECTION for the create response, the list rows and the detail read, so
// the three cannot describe the same trip three different ways — the same rule
// vehicles_list_projection.go enforces for the catalog. `addablePeopleWire` is
// here for the same reason rather than inline in its handler.

// The derived TripStatus values (trip.schema.json TripStatus). Named rather
// than spelled inline at each comparison so the query-parameter validator, the
// derivation and the wire output move together.
const (
	tripStatusScheduled = "scheduled"
	tripStatusActive    = "active"
	tripStatusEnded     = "ended"
)

// tripRoleOwner is the TripRole the store emits for an owner.
const tripRoleOwner = "owner"

// tripListMaxLimit mirrors the store's clamp. Stated here too so a bad `limit`
// is a 400 with a sentence rather than a silently clamped page.
const tripListMaxLimit = 100

// tripStatusOf derives the status at an instant.
//
// COMPUTED, NEVER STORED, and computed by the same rule the store and the SQL
// access predicate use: [startsAt, effectiveEnd), where effectiveEnd is
// min(endsAt, endedAt). A stored status would create the one state the platform
// could not explain — a row saying `active` on a window that closed an hour ago
// because a sweeper pass was missed.
func tripStatusOf(t TripData, now time.Time) string {
	// A STAMPED endedAt IS TERMINAL ON ITS OWN, matching store.Trip.StatusAt
	// arm for arm. `endsAt` is a scheduled instant; `endedAt` records that the
	// owner's end ALREADY HAPPENED, and its only writer is `SET ended_at =
	// NOW()`. Comparing it against this process's clock spans two machines and
	// reported a closed window as open for the length of the skew — 76 ms
	// against a local container, and there is no bound on it in production.
	if t.EndedAt != nil {
		return tripStatusEnded
	}
	end := t.EndsAt
	switch {
	case now.Before(t.StartsAt):
		return tripStatusScheduled
	case now.Before(end):
		return tripStatusActive
	default:
		return tripStatusEnded
	}
}

// addablePeopleWire projects §7.30.11's response envelope.
//
// It lives here beside `tripWire` rather than inline in the handler, so every
// shape this surface puts on the wire is written in one file and a reader
// checking the contract against the code has one place to look — which is the
// same reason `tripWire` is here and not in `trip_detail_handler.go`.
//
// `{people: [...]}` WITH NO CURSOR, the same envelope decision §7.30.2 makes: a
// car has a handful of share-holders, not a feed, and an SDK pagination helper
// must not mistake this for a page and go looking for a cursor that will never
// be there.
//
// **EMPTY RATHER THAN NULL**, and the `make` with a zero length is what
// guarantees it: everybody being aboard is a legitimate state of a trip the
// caller is on, and `"people": null` would make a client branch on a case that
// means exactly what the empty array means.
//
// TWO KEYS PER ROW AND NOTHING ELSE — no code, no email, no status, no
// permission, no user id. The projection that produces them is narrow in the
// store as well (see queryTripAddablePeople), so this is the second of two
// places the same restriction holds rather than the only one.
func addablePeopleWire(people []TripAddablePersonData) map[string]any {
	items := make([]map[string]any, 0, len(people))
	for _, p := range people {
		items = append(items, map[string]any{
			"shareId":     p.ShareID,
			"displayName": p.Name,
		})
	}
	return map[string]any{"people": items}
}

// tripWire projects one trip for one caller.
//
// callerID decides `userIsSelf` on each roster row and NOTHING ELSE. Everyone
// on a trip sees the whole roster — they are on a trip together, and a road
// trip whose members cannot see each other is a group chat with the names
// blanked. What the caller must not receive is anybody's USER ID, so the
// comparison happens here and the id is dropped rather than emitted.
func tripWire(t TripData, callerID string) map[string]any {
	participants := make([]map[string]any, 0, len(t.Participants))
	for _, p := range t.Participants {
		participants = append(participants, map[string]any{
			"participantId": p.ParticipantID,
			"name":          p.Name,
			"userIsSelf":    p.UserID == callerID,
			// MYR-618. A pointer with NO omitempty, the platform's convention
			// for a nullable name: the key is always present and "nobody
			// recorded who added this person" is an explicit null rather than
			// a missing field a client has to guess about. Everyone on the
			// trip sees it, because everyone on the trip can now BE the
			// answer — a roster that showed the attribution only to the owner
			// would be a private ledger of who invited whom.
			"addedByName": derefOrNil(p.AddedByName),
		})
	}

	out := map[string]any{
		"id":        t.ID,
		"vehicleId": t.VehicleID,
		"name":      t.Name,
		"startsAt":  t.StartsAt.UTC().Format(time.RFC3339),
		"endsAt":    t.EndsAt.UTC().Format(time.RFC3339),
		// A pointer with no omitempty, the platform's convention for a
		// nullable instant: the key is always present and "the owner has not
		// ended this early" is an explicit null.
		"endedAt":   formatInstantOrNil(t.EndedAt),
		"status":    tripStatusOf(t, time.Now()),
		"createdAt": t.CreatedAt.UTC().Format(time.RFC3339),
		"role":      t.Role,
		// derefOrNil for the reason the catalog gives for the same field: an
		// unset name must map to an UNTYPED nil, not a typed (*string)(nil),
		// which marshals to `null` but is not `== nil` to a test.
		"ownerFirstName": derefOrNil(t.OwnerFirstName),
		"vehicle": map[string]any{
			"vehicleId": t.Vehicle.VehicleID,
			"name":      t.Vehicle.Name,
			"model":     t.Vehicle.Model,
			"year":      t.Vehicle.Year,
			"color":     t.Vehicle.Color,
			// THE SAME HELPERS §7.0 AND §7.1 USE, called rather than
			// re-implemented, so a car cannot be named one way on its catalog
			// row and another way on a trip card.
			"vinLast4":  lastFourOfVIN(t.Vehicle.VIN),
			"trimLabel": derefOrNil(resolvedTrimLabel(t.Vehicle.Model, t.Vehicle.Year, t.Vehicle.TrimLabel, t.Vehicle.Trim, t.Vehicle.VIN)),
		},
		"participants": participants,
		"driveCount":   t.DriveCount,
		// MYR-608. ALWAYS PRESENT, NULL WHEN THE WINDOW HOLDS NO DRIVES — the
		// platform's nullable convention, and the same one `endedAt` above
		// follows. Null is `SUM` over zero rows, deliberately not coalesced to
		// `0`: zero miles is a real total for a window whose drives went
		// nowhere, and a client that could not tell the two apart would print
		// "0 mi" on a trip that has not begun.
		//
		// SENT WHILE THE TRIP IS ACTIVE, not withheld until it ends. They are
		// RUNNING totals — read at read time like `driveCount`, climbing
		// between reads — and the surface that most wants a total is a road
		// trip in progress. This is what deletes the client's own window
		// arithmetic and its "withhold when the page is partial" rule: the
		// server sums the WHOLE window, so a client holding one page of it can
		// state the total anyway.
		"totalDistanceMiles":   derefOrNil(t.TotalDistanceMiles),
		"totalDurationSeconds": tripTotalDurationSeconds(t.TotalDurationMinutes),
		// MYR-629. THE SAME ALWAYS-PRESENT-AND-NULLABLE SHAPE as its two
		// siblings above, and A STRICTER NULL. Theirs means "the window holds no
		// drives"; this one ALSO means "at least one drive that moved reported
		// no energy", because `Drive."energyUsedKwh"` is NOT NULL and an
		// unmeasurable drive writes 0 there rather than null.
		//
		// A CLIENT RENDERS EFFICIENCY AS `totalEnergyKwh × 1000 /
		// totalDistanceMiles` and shows its "not reported" dash on null. THE
		// SERVER STORES NO RATIO and never will: a persisted Wh/mi could
		// disagree with the two numbers on the same card that produced it.
		"totalEnergyKwh": derefOrNil(t.TotalEnergyKwh),
	}

	// `currentLeg` is OPTIONAL on the contract and ABSENT rather than null when
	// there is none — the one field on this shape where absence is the
	// spelling. It is informational and never a gate, so a consumer that reads
	// its absence as "nothing is being driven right now" is correct, and one
	// that reads it as "the trip is not live" is wrong: an active trip with no
	// leg is the ordinary overnight state.
	if t.CurrentLeg != nil {
		out["currentLeg"] = map[string]any{
			"destinationName": t.CurrentLeg.DestinationName,
			"etaMinutes":      derefIntOrNil(t.CurrentLeg.EtaMinutes),
			"startedAt":       t.CurrentLeg.StartedAt.UTC().Format(time.RFC3339),
		}
	}
	return out
}

// tripTotalDurationSeconds converts the stored MINUTES sum to the contract's
// SECONDS, preserving null (MYR-608).
//
// THE CONVERSION HAPPENS HERE, at the same boundary `buildDriveSummary` turns
// `durationMinutes` into `durationSeconds`, and for the same reason: the Prisma
// column is minutes and the wire contract is seconds, so exactly one place in
// the process may know that. A total in minutes and a per-drive figure in
// seconds on the same response would be the arithmetic trap this whole issue
// exists to remove.
//
// int64 throughout: the sum is a Postgres bigint, and a window is capped at 30
// days but the SUM is over rows, not over the window.
func tripTotalDurationSeconds(minutes *int64) any {
	if minutes == nil {
		return nil
	}
	return *minutes * 60
}

// derefIntOrNil is derefOrNil for an int pointer: an untyped nil rather than a
// typed (*int)(nil), which marshals to `null` but is not `== nil` to a test.
func derefIntOrNil(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
