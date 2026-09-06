// The OPS half of MYR-599: the one read that carries Tesla's RAW
// `tesla_access_type` token out of the table, and the one place the operator
// vocabulary for it is spelled.
//
// WHY THIS IS A SEPARATE READ AND NOT A WIDER CATALOG PROJECTION.
// catalogDriverAccessExpr deliberately selects two timestamps and NOT the raw
// token: the §7.0 wire enum has two members, row presence decides between them,
// and a column no response body emits is exactly what the AGENTS.md
// lean-projection invariant forbids on the REST list path. The raw token exists
// for a different question — "what did Tesla actually say about this person's
// access to this car?" — which only an operator ever asks, and which no hot
// query should pay for. So it leaves the table through this read, used by
// `ops vehicles list` and by cmd/sweep-orphan-fleet-configs, and through no
// other.
//
// A READ of the Prisma-owned "Vehicle" LEFT JOINed to a Go-owned table, reusing
// driverAccessJoin verbatim so the ops surfaces and the catalog cannot disagree
// about what "has a driver-access row" means. CG-DL-9 governs MIGRATIONS, not
// runtime SELECTs.

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Operator access labels. FOUR members, not the wire enum's two, and the extra
// pair is the whole reason this vocabulary is separate from the §7.0
// `teslaAccessType` the handlers emit:
//
//   - the wire enum answers "owner or driver", which is all a client can act on;
//   - an operator additionally needs to see the SHUT GATE (a driver row nobody
//     has acknowledged yet is why a car is not streaming), and needs to be told
//     when the question could not be answered at all rather than being shown a
//     confident "owner" that was really an absent vehicle row.
//
// Spelled once, here, because two binaries print them and a vocabulary spelled
// in two files is a vocabulary that will eventually be spelled two ways.
const (
	// OperatorAccessOwner: a "Vehicle" row exists and carries no driver-access
	// row. The ordinary case, and every car in the fleet before MYR-599.
	OperatorAccessOwner = "owner"
	// OperatorAccessDriver: a driver-access row exists and has been
	// acknowledged. The steady state after the sheet — the car streams like any
	// other and is still not owned by the person who linked it.
	OperatorAccessDriver = "driver"
	// OperatorAccessDriverUnacknowledged: a driver-access row exists with
	// acknowledged_at NULL. THE PUSH GATE IS SHUT: no fleet-telemetry config
	// will be pushed at this car until the driver acknowledges, so this is the
	// label that explains an otherwise-healthy car that never streams.
	OperatorAccessDriverUnacknowledged = "driver(unacknowledged)"
	// OperatorAccessUnknown: NO "Vehicle" row was found for this (owner, VIN)
	// pair, so there is no vehicle id to look an access row up by and the
	// question is unanswerable — NOT an assertion of owner access. This is the
	// ORDINARY label in the orphan sweep, whose candidates are VINs no local
	// vehicle row claims; see cmd/sweep-orphan-fleet-configs/doc.go.
	OperatorAccessUnknown = "unknown"
)

// DriverAccessListing is one ops-listing row: the domain VehicleDriverAccess,
// widened with the two facts only an operator surface is allowed to see.
//
// Embeds rather than copies VehicleDriverAccess so Present / CreatedAt /
// AcknowledgedAt keep exactly one definition and PendingAcknowledgment — the
// gate every push path consults — is the same method here as everywhere else.
type DriverAccessListing struct {
	VehicleDriverAccess

	// VehicleFound reports that a "Vehicle" row was located for the (owner,
	// VIN) pair this listing was asked about.
	//
	// LOAD-BEARING, and the reason this type exists at all: the zero value of
	// VehicleDriverAccess reads as OWNER ACCESS, which is the right default for
	// a car we hold, and a dangerous lie for one we do not. A map miss therefore
	// yields VehicleFound=false and OperatorLabel() reports `unknown`, so the
	// two states — "we looked and there is no driver row" and "there was nothing
	// to look at" — can never be printed as the same thing.
	VehicleFound bool

	// TeslaAccessType is Tesla's access_type VERBATIM, as it was stored: usually
	// "DRIVER", and the EMPTY STRING for older Fleet API responses that shipped
	// no access_type at all (RecordDriverAccess stores absence as absence rather
	// than inventing a value). Empty also, trivially, when there is no row.
	//
	// Printed unchanged. The point of the column is to answer what Tesla said,
	// and normalising it here would erase the answer.
	TeslaAccessType string
}

// OperatorLabel renders the listing as one of the four labels above.
//
// The zero value answers `unknown`, which is what makes
// `m[vin].OperatorLabel()` correct on a map miss without the caller restating
// the absent-row rule at each call site (there are two binaries and three call
// sites).
func (a DriverAccessListing) OperatorLabel() string {
	switch {
	case !a.VehicleFound:
		return OperatorAccessUnknown
	case !a.Present:
		return OperatorAccessOwner
	case a.PendingAcknowledgment():
		return OperatorAccessDriverUnacknowledged
	default:
		return OperatorAccessDriver
	}
}

// queryDriverAccessListingByVIN resolves a batch of one owner's VINs to their
// driver-access rows.
//
// SCOPED TO THE OWNER, matching queryUpsertDriverAccessByVIN's `"userId" = $2`
// guard exactly. A driver-access row is a fact about ONE person's relationship
// to one car, so resolving a VIN across all users could attach somebody else's
// row to this listing's line — and a VIN that belongs to a different account is
// precisely a case the operator must see as `unknown`, not as an answer.
//
// LEFT JOIN, so a car with no driver row still comes back and is reported
// `owner`. An INNER JOIN would collapse `owner` into `unknown` and make the
// listing useless.
//
// = ANY($2) over a text[] rather than a generated IN list: one parameter, one
// plan, and no string-built SQL (raw SQL with pgx, parameterised only).
const queryDriverAccessListingByVIN = `
SELECT "Vehicle"."vin", COALESCE(dva.tesla_access_type, ''), ` + catalogDriverAccessExpr + `
FROM "Vehicle"` + driverAccessJoin + `
WHERE "Vehicle"."userId" = $1 AND "Vehicle"."vin" = ANY($2)`

// ListDriverAccessByVIN returns the driver-access listing for each of userID's
// vins, keyed by VIN.
//
// VINs WITH NO "Vehicle" ROW ARE SIMPLY ABSENT from the map — deliberately, not
// as an oversight: the caller reads a miss as the zero DriverAccessListing,
// whose OperatorLabel is `unknown`. Absence is the answer, so there is nothing
// for this method to invent.
//
// An empty vin list or an empty userID is not an error; it returns an empty map
// and issues no query, so an ops surface with nothing to annotate costs no
// round trip.
func (r *VehicleRepo) ListDriverAccessByVIN(
	ctx context.Context, userID string, vins []string,
) (map[string]DriverAccessListing, error) {
	out := make(map[string]DriverAccessListing, len(vins))
	if strings.TrimSpace(userID) == "" || len(vins) == 0 {
		return out, nil
	}

	start := time.Now()
	rows, err := r.pool.Query(ctx, queryDriverAccessListingByVIN, userID, vins)
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_driver_access_by_vin")
		return nil, fmt.Errorf("VehicleRepo.ListDriverAccessByVIN(%s): %w", userID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			vin     string
			rawType string
			acc     driverAccessScan
		)
		// The timestamp pair is scanned through driverAccessScan rather than
		// into two local pointers, so this read shares the catalog's column
		// order AND its NULL semantics (a NULL created_at is the LEFT JOIN
		// finding no row) instead of restating them.
		dests := acc.dests()
		scanDest := make([]any, 0, 2+len(dests))
		scanDest = append(scanDest, &vin, &rawType)
		scanDest = append(scanDest, dests...)
		if err := rows.Scan(scanDest...); err != nil {
			r.metrics.IncQueryError("vehicle.list_driver_access_by_vin")
			return nil, fmt.Errorf("VehicleRepo.ListDriverAccessByVIN(%s): scan: %w", userID, err)
		}
		out[vin] = DriverAccessListing{
			VehicleDriverAccess: acc.value(),
			VehicleFound:        true,
			TeslaAccessType:     rawType,
		}
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("vehicle.list_driver_access_by_vin")
		return nil, fmt.Errorf("VehicleRepo.ListDriverAccessByVIN(%s): rows: %w", userID, err)
	}

	r.metrics.ObserveQueryDuration("vehicle.list_driver_access_by_vin", time.Since(start).Seconds())
	return out, nil
}
