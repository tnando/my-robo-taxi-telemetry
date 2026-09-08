package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"
)

// MYR-608 — WHAT THE WIRE SAYS, as opposed to what the SQL resolves.
//
// The store tests (internal/store/trip_drive_totals_test.go) pin the window
// arithmetic against a real database. These pin the two things a database
// cannot: that the keys are ALWAYS PRESENT, and that "no value" is spelled
// `null` rather than an absent key or a zero.
//
// THE DISTINCTION IS THE WHOLE FEATURE. A client that cannot tell "this server
// has no total for you" from "this server does not send totals" has to keep the
// arithmetic it was told to delete, and would silently fall back to it against
// a deployed server that simply had nothing to sum.

// fixtureTripWithTotals is fixtureTrip carrying the two sums.
func fixtureTripWithTotals(miles float64, minutes int64) TripData {
	trip := fixtureTrip()
	trip.TotalDistanceMiles = &miles
	trip.TotalDurationMinutes = &minutes
	return trip
}

// TestTripCarriesRunningTotals covers the populated case on both §7.30.3 and
// §7.30.2, and the minutes → seconds conversion.
//
// THE CONVERSION HAPPENS AT THIS BOUNDARY, the same one `durationSeconds` is
// converted at on a drive row. A total in minutes beside per-drive figures in
// seconds would be the arithmetic trap this issue exists to remove.
func TestTripCarriesRunningTotals(t *testing.T) {
	trip := fixtureTripWithTotals(128.4, 205)

	t.Run("the detail read", func(t *testing.T) {
		handler := newTripTestHandler(t, &fakeTripStore{trip: trip}, true)
		body := decodeTripBody(t, handler, "/api/trips/"+tripTestID)

		if got := body["totalDistanceMiles"]; got != 128.4 {
			t.Errorf("totalDistanceMiles = %v, want 128.4", got)
		}
		// 205 minutes is 12300 seconds. JSON numbers decode as float64.
		if got := body["totalDurationSeconds"]; got != float64(12300) {
			t.Errorf("totalDurationSeconds = %v, want 12300 (205 minutes)", got)
		}
	})

	t.Run("every row of the list", func(t *testing.T) {
		// ONE PROJECTION serves the create response, the list rows and the
		// detail read, so a field that reached one reaches all three. This is
		// the assertion that keeps that true for the totals.
		handler := newTripTestHandler(t, &fakeTripStore{trip: trip}, true)
		rec := tripRequest(t, handler, http.MethodGet, "/api/trips", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		var envelope struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(envelope.Items) != 1 {
			t.Fatalf("items = %v, want one row", envelope.Items)
		}
		if got := envelope.Items[0]["totalDistanceMiles"]; got != 128.4 {
			t.Errorf("list row totalDistanceMiles = %v, want 128.4", got)
		}
		if got := envelope.Items[0]["totalDurationSeconds"]; got != float64(12300) {
			t.Errorf("list row totalDurationSeconds = %v, want 12300", got)
		}
	})
}

// TestTripTotalsAreAlwaysPresentAndNullWhenEmpty is the always-present-nullable
// rule, and it is not a style preference.
//
// A key described as always present but permitted to be absent is a promise the
// contract does not make: a strict decoder is entitled to accept a row without
// it, and a client would be unable to tell an empty window from a server that
// predates the field. `null` says "the server looked and there is nothing to
// sum"; a missing key says nothing at all.
//
// AND NULL IS NOT ZERO. `SUM` over no rows is NULL, carried through rather than
// coalesced, because `0` is a legitimate total for a window whose drives went
// nowhere — and printing "0 mi" on a trip that has not begun is exactly the
// wrong answer.
func TestTripTotalsAreAlwaysPresentAndNullWhenEmpty(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true)
	body := decodeTripBody(t, handler, "/api/trips/"+tripTestID)

	for _, key := range []string{"totalDistanceMiles", "totalDurationSeconds"} {
		value, present := body[key]
		if !present {
			t.Errorf("%s is absent; the contract declares it always present and nullable", key)
			continue
		}
		if value != nil {
			t.Errorf("%s = %v for a window with nothing to sum, want null (and NOT 0)", key, value)
		}
	}
}

// TestTripWireStillCarriesEveryDeclaredKey guards the shape against a field
// being added in one projection and forgotten in another.
func TestTripWireStillCarriesEveryDeclaredKey(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTripWithTotals(1, 1)}, true)
	body := decodeTripBody(t, handler, "/api/trips/"+tripTestID)

	for _, key := range []string{
		"id", "vehicleId", "name", "startsAt", "endsAt", "endedAt", "status",
		"createdAt", "role", "ownerFirstName", "vehicle", "participants",
		"driveCount", "totalDistanceMiles", "totalDurationSeconds",
	} {
		if _, present := body[key]; !present {
			t.Errorf("the trip shape is missing the required key %q", key)
		}
	}
}

// decodeTripBody issues one GET and decodes its object body.
func decodeTripBody(t *testing.T, handler http.Handler, path string) map[string]any {
	t.Helper()
	rec := tripRequest(t, handler, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}
