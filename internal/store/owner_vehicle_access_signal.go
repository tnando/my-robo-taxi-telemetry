// The TRI-STATE the provisioning transaction reads its consent gate from
// (MYR-599), split from owner_vehicle_provision.go so the type and its whole
// argument sit together.
//
// ── WHY A BOOL WAS THE WRONG SHAPE, AND WHAT IT COST ────────────────────────
//
// OwnedVehicleInput used to carry `IsOwnerAccess bool`, derived by the caller
// from Tesla's `access_type` with an explicit fail-closed rule: anything that is
// not "OWNER" — including the EMPTY STRING older Fleet API responses have
// shipped — is not owner access.
//
// That rule is right at the boundary and dangerous as a Go zero value. `false`
// means DRIVER, so every hand-built input, every fixture and every future caller
// that forgets the field asks the transaction to GATE the car. On a fresh insert
// that is the safe direction. On an ESTABLISHED OWNER'S ROW it is a catastrophe
// wearing a fail-closed costume: one Fleet listing that omitted `access_type`
// for a car its real owner has been streaming for months would file a
// driver-access row against it, and the next thing anybody would notice is that
// a paying customer's car stopped streaming and their app showed a sheet asking
// them to confirm somebody else approved it.
//
// So the signal is now a THREE-valued type whose zero value is UNKNOWN, and
// unknown means DO NOT TOUCH THE GATES — neither open one nor shut one. There is
// no longer a value a forgetful caller can accidentally spell that changes a
// car's consent state.
//
// The fail-closed reading itself is unchanged and still lives with the caller:
// AccessSignalFor maps every non-OWNER answer Tesla has ever given, empty
// included, onto AccessSignalDriver. What changed is that "the caller said
// nothing" is no longer spelled the same way as "the caller said driver".

package store

import "strings"

// TeslaAccessSignal is what the caller learned about the linking account's
// relationship to one car, as an explicit three-valued fact.
//
// It is NOT Tesla's raw `access_type` — that string is carried separately on
// OwnedVehicleInput and stored verbatim, because the column exists to answer
// "what did Tesla actually say?" and normalising it would erase the answer.
// This type is the INTERPRETATION, and it exists so the transaction can tell
// "Tesla said something other than OWNER" from "nobody told us anything".
type TeslaAccessSignal string

const (
	// AccessSignalUnknown is the ZERO VALUE and means the caller made no claim.
	// The provisioning transaction leaves the driver-access row exactly as it
	// found it — it neither writes a gate nor clears one — and reports the row's
	// existing state honestly so a caller reading the result is not told a car
	// is ungated merely because nobody looked.
	AccessSignalUnknown TeslaAccessSignal = ""
	// AccessSignalOwner: Tesla reports the linking account as this car's OWNER.
	// Any driver-access row is cleared (the access-UPGRADE case), and a
	// cross-user conflict against a DRIVER-provisioned row transfers the car —
	// see owner_vehicle_transfer.go.
	AccessSignalOwner TeslaAccessSignal = "owner"
	// AccessSignalDriver: Tesla reports something OTHER than OWNER, including
	// saying nothing at all. Fail closed — an unknown access level is never
	// promoted to ownership — but see applyDriverAccess for the one place that
	// closure is bounded: it may create a gate, never convert an established
	// owner's car into a gated one.
	AccessSignalDriver TeslaAccessSignal = "driver"
)

// AccessSignalFor maps Tesla's raw `access_type` onto the signal.
//
// ONE SPELLING OF THE FAIL-CLOSED RULE, exported so the link-time hook and any
// future caller share it rather than each re-deriving "is this OWNER?" — a rule
// re-derived in two places is a rule that will eventually disagree with itself,
// and here the disagreement would open a consent gate.
//
// The EMPTY string maps to driver, deliberately: older Fleet API responses have
// shipped no access_type at all, and an absent access level must not be read as
// an assertion of ownership. That is not the same as AccessSignalUnknown, which
// means the CALLER never looked — Tesla answering with silence is still Tesla
// answering.
func AccessSignalFor(rawAccessType string) TeslaAccessSignal {
	if strings.EqualFold(strings.TrimSpace(rawAccessType), "OWNER") {
		return AccessSignalOwner
	}
	return AccessSignalDriver
}
