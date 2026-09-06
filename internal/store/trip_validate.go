package store

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// INPUT VALIDATION for MYR-602 trips, split out of trip_repo.go under the
// 300-line file cap.
//
// Both rules are stated TWICE on the platform — here and as a CHECK constraint
// in migration 0047 — and the duplication is deliberate. The constraint is the
// one that cannot be bypassed by a future writer; this copy is what lets the
// API answer 400 with a sentence a person can act on, rather than 500 with a
// constraint violation. A validation that lives in only one of two writers is a
// validation that holds half the time.

// ValidateTripName trims and checks a proposed trip name, returning the value
// to store.
//
// RUNES, NOT BYTES. A 60-character name in any script must be accepted, and a
// byte cap would silently refuse one written in Japanese while accepting the
// same length in English. Same rule and same reasoning as MYR-581's
// PATCH /api/users/me.
//
// Exported so the handler layer validates with the identical function rather
// than a second implementation of the same three rules.
func ValidateTripName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("%w: name is empty after trimming", ErrTripNameInvalid)
	}
	if utf8.RuneCountInString(name) > MaxTripNameLen {
		return "", fmt.Errorf("%w: name is longer than %d characters", ErrTripNameInvalid, MaxTripNameLen)
	}
	for _, r := range name {
		// Control characters are nothing a keyboard types and everything a
		// log-injection or a broken renderer does. Refused here, once, so no
		// consumer has to defend against one.
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: name contains a control character", ErrTripNameInvalid)
		}
	}
	return name, nil
}

// validateTripWindow enforces the two rules the go_trips CHECK constraints also
// carry.
//
// STATED TWICE ON PURPOSE. The constraint is the one that cannot be bypassed;
// this copy exists so the API answers 400 with a sentence rather than 500 with
// a constraint violation, and so a future writer that forgets the constraint
// still cannot create a bad window. A validation that lives in only one of two
// writers is a validation that holds half the time.
//
// A WINDOW MAY START IN THE PAST and that is deliberately NOT checked: it is
// how the legs of a road trip already driven join the trip, and it is a stated
// product requirement rather than an oversight to guard against.
func validateTripWindow(startsAt, endsAt time.Time) error {
	if !endsAt.After(startsAt) {
		return fmt.Errorf("%w: endsAt must be after startsAt", ErrTripWindowInvalid)
	}
	if endsAt.Sub(startsAt) > MaxTripWindow {
		return fmt.Errorf("%w: a trip may not exceed %d days", ErrTripWindowInvalid, int(MaxTripWindow.Hours()/24))
	}
	return nil
}
