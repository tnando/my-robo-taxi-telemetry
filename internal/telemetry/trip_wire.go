package telemetry

import (
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The WIRE PROJECTION for §7.30 — the one function that turns a TripData into
// the `Trip` shape of trip.schema.json, plus the request bodies and the derived
// status.
//
// ONE PROJECTION for the create response, the list rows and the detail read, so
// the three cannot describe the same trip three different ways — the same rule
// vehicles_list_projection.go enforces for the catalog.

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
	end := t.EndsAt
	if t.EndedAt != nil && t.EndedAt.Before(end) {
		end = *t.EndedAt
	}
	switch {
	case now.Before(t.StartsAt):
		return tripStatusScheduled
	case now.Before(end):
		return tripStatusActive
	default:
		return tripStatusEnded
	}
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

// derefIntOrNil is derefOrNil for an int pointer: an untyped nil rather than a
// typed (*int)(nil), which marshals to `null` but is not `== nil` to a test.
func derefIntOrNil(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// createTripBody is CreateTripRequest (trip.schema.json). Every field is
// REQUIRED on the contract except `participantIds`, which may be empty — an
// owner may open a window and add people later.
//
// The instants are decoded as STRINGS and parsed explicitly rather than into
// time.Time, so a malformed date is a 400 naming the field instead of the
// decoder's own message about the whole body.
type createTripBody struct {
	Name           string   `json:"name"`
	StartsAt       string   `json:"startsAt"`
	EndsAt         string   `json:"endsAt"`
	ParticipantIDs []string `json:"participantIds"`
}

// parseCreate validates and converts the body.
//
// `vehicleId` is deliberately NOT read from the body even though
// CreateTripRequest carries it: the PATH is the authority, and a request whose
// body named a different car would otherwise have two answers to which car it
// is about. The schema keeps the field for the SDKs' single-shape typing.
func (h *TripHandler) parseCreate(w http.ResponseWriter, vehicleID, userID string, body createTripBody) (TripCreateInput, bool) {
	startsAt, ok := h.parseInstant(w, "startsAt", body.StartsAt)
	if !ok {
		return TripCreateInput{}, false
	}
	endsAt, ok := h.parseInstant(w, "endsAt", body.EndsAt)
	if !ok {
		return TripCreateInput{}, false
	}
	return TripCreateInput{
		VehicleID:   vehicleID,
		OwnerUserID: userID,
		// NOT VALIDATED HERE. The store owns the name rules (trimmed, 1..60
		// RUNES, no control characters) and applies them to the create and the
		// patch through one function; a copy here would be a second rule to
		// drift.
		Name:                body.Name,
		StartsAt:            startsAt,
		EndsAt:              endsAt,
		ParticipantShareIDs: body.ParticipantIDs,
	}, true
}

// updateTripBody is UpdateTripRequest. Pointers so an ABSENT key ("leave this
// alone") is distinguishable from a present one — the distinction PATCH is
// entirely built on.
type updateTripBody struct {
	Name                 *string  `json:"name"`
	EndsAt               *string  `json:"endsAt"`
	AddParticipantIDs    []string `json:"addParticipantIds"`
	RemoveParticipantIDs []string `json:"removeParticipantIds"`
}

func (h *TripHandler) parseUpdate(w http.ResponseWriter, body updateTripBody) (TripUpdateInput, bool) {
	in := TripUpdateInput{
		Name:                 body.Name,
		AddParticipantIDs:    body.AddParticipantIDs,
		RemoveParticipantIDs: body.RemoveParticipantIDs,
	}
	if body.EndsAt != nil {
		endsAt, ok := h.parseInstant(w, "endsAt", *body.EndsAt)
		if !ok {
			return TripUpdateInput{}, false
		}
		in.EndsAt = &endsAt
	}
	return in, true
}

// parseInstant reads one RFC 3339 field, naming it in the refusal.
func (h *TripHandler) parseInstant(w http.ResponseWriter, field, raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			field+" must be an RFC 3339 instant")
		return time.Time{}, false
	}
	return t, true
}
