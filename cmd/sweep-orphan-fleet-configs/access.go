// MYR-599's column on the orphan report: what kind of access each reported VIN
// was linked under, and what Tesla actually said about it.
//
// WHY THIS LIVES IN cmd AND NOT IN internal/fleetorphan. The sweep package
// deliberately imports neither internal/store nor internal/telemetry — every
// collaborator it has is a consumer-site interface so it stays trivially
// fakeable — and the driver-access row is a store read. So the annotation is a
// cmd-side pass over the finished report, exactly like the adapters in
// adapters.go: a boundary translation with no decision in it.
//
// ── READ THIS BEFORE YOU READ THE COLUMN ────────────────────────────────────
//
// MOST ROWS OF THIS REPORT WILL SAY `unknown`, and that is correct rather than
// broken. Both candidate sources are, BY CONSTRUCTION, VINs that no local
// "Vehicle" row claims — that is what makes them orphans — and a driver-access
// row is keyed by "Vehicle"."id". No vehicle row means no id, which means
// nothing to look the access row up by. Worse, the row itself is usually gone
// too: owner teardown deletes the car's go_vehicle_driver_access row with the
// car (internal/store/owner_teardown.go) and account deletion deletes the
// person's rows with the account, so for a tombstoned orphan there is typically
// neither end of the join left.
//
// The column is still worth printing for the case that DOES resolve: a VIN
// Tesla listed for a live owner whose "Vehicle" row still exists under that same
// owner — a car mid-teardown, or one whose local row outlived a failed removal.
// That is the row where "was this a driver car, and did the driver ever
// acknowledge?" changes what an operator does next.
//
// What `unknown` NEVER means is `owner`. Keeping those two labels distinct is
// the whole reason DriverAccessListing carries VehicleFound.

package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/myrobotaxi/telemetry/internal/fleetorphan"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// driverAccessLister is the consumer-site view of *store.VehicleRepo used here:
// one owner-scoped batch read, so a test can drive the annotation without a
// database.
type driverAccessLister interface {
	ListDriverAccessByVIN(
		ctx context.Context, userID string, vins []string,
	) (map[string]store.DriverAccessListing, error)
}

// reportedVIN is one line of the annotated per-VIN report: fleetorphan's own
// line plus the MYR-599 access columns.
//
// The embedded VINOutcome is INLINED by encoding/json (an embedded struct with
// no tag contributes its fields at the outer level), so vin/userId/sources/
// outcome/detail/configExp keep their exact existing names, order and
// semantics, with the two new keys appended after them. This type only ever
// ADDS keys, so a report saved before MYR-599 still diffs cleanly against one
// saved after.
type reportedVIN struct {
	fleetorphan.VINOutcome
	// Access is the operator label: `owner`, `driver`, `driver(unacknowledged)`
	// or — usually, on this report — `unknown`. Always present.
	Access string `json:"access"`
	// TeslaAccessType is Tesla's raw access_type token, omitted when there is
	// no driver-access row to quote or when the row carries no token (an older
	// Fleet API response that shipped none).
	TeslaAccessType string `json:"teslaAccessType,omitempty"`
}

// accessReport is the whole run with the annotated VIN lines substituted.
//
// THE SHADOWING IS DELIBERATE AND IS THE POINT OF THE TYPE. `VINs` here sits at
// depth 0 and the promoted `fleetorphan.Report.VINs` at depth 1, so
// encoding/json's shallowest-wins rule emits exactly one `"vins"` key — this
// one. Every other field of the report (dryRun, counts, attempts, tombstones,
// errors) is promoted untouched, so adding a field to fleetorphan.Report shows
// up here for free instead of silently dropping out of the operator's artifact,
// which is what a hand-copied struct would have done. The one visible cost is
// that `vins` now sorts LAST at the top level rather than third — the object's
// key order, not its content, and no reader of a JSON object may depend on it.
//
// The embedded copy's own VINs slice is left populated rather than nil'd: it
// costs nothing, it is unreachable through the encoder, and the alternative
// discards the un-annotated truth to guard against a rule the test below pins.
type accessReport struct {
	fleetorphan.Report
	VINs []reportedVIN `json:"vins"`
}

// annotateDriverAccess resolves the access type of every reported VIN.
//
// ONE BATCH READ PER OWNER, not one per VIN: the report groups naturally by
// userId (it is how the token was resolved in the first place), and the store
// read is owner-scoped for the same reason the write path is — a driver-access
// row is a fact about one person's relationship to one car, and resolving a VIN
// across accounts could attach somebody else's row to this line.
//
// A LOOKUP FAILURE IS NEVER FATAL. It lands in the report's Errors block, the
// affected lines stay `unknown`, and the run still prints — matching the
// package's own contract that a sweep which examined half the fleet is worth
// having, and that per-VIN trouble belongs IN the report rather than in an exit
// code. This annotation must never be the reason an operator loses a report
// whose Tesla work already succeeded.
func annotateDriverAccess(
	ctx context.Context, lister driverAccessLister, rep fleetorphan.Report,
) accessReport {
	out := accessReport{Report: rep, VINs: make([]reportedVIN, 0, len(rep.VINs))}

	byUser := make(map[string][]string, len(rep.VINs))
	for _, v := range rep.VINs {
		// A line with no owner handle has nothing to scope a lookup to. It
		// stays `unknown`, which is precisely what it is.
		if v.UserID == "" {
			continue
		}
		byUser[v.UserID] = append(byUser[v.UserID], v.VIN)
	}

	// Owner-sorted so two runs against the same data queue their reads — and
	// any errors they produce — in the same order, keeping the artifact
	// diffable. Same reason the package sorts its VIN lines.
	users := make([]string, 0, len(byUser))
	for userID := range byUser {
		users = append(users, userID)
	}
	sort.Strings(users)

	// Keyed by owner AND VIN: two owners can each be reported for the same VIN
	// (one holds the car, the other has a stale tombstone for it), and their
	// access answers are different facts that must not overwrite each other.
	resolved := make(map[[2]string]store.DriverAccessListing, len(rep.VINs))
	for _, userID := range users {
		found, err := lister.ListDriverAccessByVIN(ctx, userID, byUser[userID])
		if err != nil {
			// No VIN in the message: the owner handle names the failure well
			// enough, and the report's VIN lines are the one authoritative copy.
			out.Errors = append(out.Errors,
				fmt.Sprintf("user %s: list driver access: %s", userID, truncate(err.Error())))
			continue
		}
		for vin, listing := range found {
			resolved[[2]string{userID, vin}] = listing
		}
	}

	for _, v := range rep.VINs {
		// The zero DriverAccessListing on a miss carries VehicleFound=false, so
		// OperatorLabel answers `unknown` without this loop restating the rule.
		listing := resolved[[2]string{v.UserID, v.VIN}]
		out.VINs = append(out.VINs, reportedVIN{
			VINOutcome:      v,
			Access:          listing.OperatorLabel(),
			TeslaAccessType: listing.TeslaAccessType,
		})
	}
	return out
}

// errorTextCap bounds an error string added to the report, mirroring the cap
// fleetorphan applies to its own error lines so one verbose driver error cannot
// bury the run's real findings.
const errorTextCap = 200

// truncate caps an error string at errorTextCap runes' worth of bytes, marking
// that it was cut so nobody reads a clipped message as the whole story.
func truncate(s string) string {
	if len(s) <= errorTextCap {
		return s
	}
	return s[:errorTextCap] + "…(truncated)"
}
