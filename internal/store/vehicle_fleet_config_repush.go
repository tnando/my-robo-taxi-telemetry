// Candidate listing for the MYR-630 fleet-config RE-PUSH sweep.
//
// ── WHY A SECOND CANDIDATE QUERY EXISTS ─────────────────────────────────────
//
// queryFleetConfigCandidates (vehicle_fleet_config_candidates.go) lists the
// cars that are NOT streaming, so the MYR-448 reconciler can heal them. This
// one lists the exact complement: the cars that ARE streaming, because a change
// to DefaultFieldConfig reaches a car only when the config is pushed again and
// nothing re-pushes a healthy car. There is no config version or hash at Tesla
// that would let a car notice it is running an old field set, so "already
// configured" and "configured with the CURRENT field set" are indistinguishable
// from here — which is why the sweep is an operator action rather than a
// background pass.
//
// ── WHAT IS EXCLUDED IN SQL, AND WHAT IS ONLY REPORTED ──────────────────────
//
// Excluded outright, both for the same reason the reconciler excludes them —
// these are not judgement calls the operator gets to override:
//
//   - a tombstoned car (MYR-261 tombstone-wins),
//   - a car linked by a DRIVER whose owner-approval acknowledgment is
//     outstanding (MYR-599 consent-wins).
//
// Everything else is REPORTED rather than filtered, because this tool's first
// job is a dry run an operator reads. A suspended car and a car whose config
// never landed are both legitimate rows to see and refuse; silently dropping
// them would make "the sweep pushed 6 of 9" unexplainable. The refusal itself
// lives in internal/fleetrepush, next to the reasons it prints.
//
// PendingOwnerAck is projected anyway, exactly as queryFleetConfigCandidates
// projects it: every producer reports the fact and the consumer enforces it, so
// the gate cannot be re-opened by adding a producer. See pendingOwnerAckExprV.
//
// A READ of the Prisma-owned "Vehicle" and side tables; no writes at all.

package store

import (
	"context"
	"fmt"
	"time"
)

// StreamingFleetVehicle is one car the re-push sweep may act on, plus every
// fact the sweep needs to decide whether it should.
type StreamingFleetVehicle struct {
	// VehicleID is the Prisma "Vehicle"."id" cuid.
	VehicleID string
	// VIN is what the config push is addressed to.
	VIN string
	// UserID owns the car, and owns the Tesla token the push authenticates with.
	UserID string
	// VehicleName is the owner's nickname for the car, for the operator's
	// listing. May be empty.
	VehicleName string
	// LastUpdated is the row's last write of any kind — the staleness hint that
	// says whether this car has streamed recently. Reported, never a filter:
	// a parked car that has been asleep for a week is still configured and
	// still needs the new field set.
	LastUpdated time.Time
	// Status is "Vehicle"."status", the motion value a streamed frame folds in.
	Status string
	// Suspended is true when MYR-592 removed this car's config for owner
	// inactivity. Its config is GONE at Tesla, so a re-push would silently
	// undo a cost-control decision — the sweep refuses it.
	Suspended bool
	// ConfigAbsent is true when go_fleet_config_attempts says the last push did
	// not take (awaiting_virtual_key, push_failed, …). There is no config to
	// refresh and the MYR-448 reconciler already owns the retry.
	ConfigAbsent bool
	// PendingOwnerAck mirrors FleetConfigCandidate.PendingOwnerAck: always
	// false here by construction of the anti-join below, projected so this
	// producer states the fact rather than assuming it.
	PendingOwnerAck bool
}

// queryStreamingFleetConfigVehicles lists cars that plausibly hold a live
// fleet-telemetry config, most recently active first.
//
// $1 = row cap.
//
// ORDERED BY "lastUpdated" DESC because when a cap truncates the run, the cars
// worth reaching first are the ones actually streaming — a change to the field
// set has no observable effect on a car that has not connected in a month until
// it next wakes, whereas the fleet's live cars start emitting the new cadence
// within the interval.
//
// The "Account" semi-join is NOT here, unlike the reconciler's query. A car
// whose owner holds no tesla row cannot be pushed either, but the sweep would
// rather show that car and label it `no_token` than drop it silently: an
// operator running a fleet-wide sweep is entitled to a report whose row count
// matches the fleet. The token resolve fails cheaply and costs no Tesla call.
const queryStreamingFleetConfigVehicles = `
SELECT v."id", v."vin", v."userId", COALESCE(v."name", ''), v."lastUpdated", v."status",
       (s.suspended_at IS NOT NULL),
       COALESCE(fa.last_outcome, '') IN ` + fleetConfigAbsentOutcomes + `,
       ` + pendingOwnerAckExprV + `
FROM "Vehicle" v
LEFT JOIN go_vehicle_telemetry_suspensions s ON s.vehicle_id = v."id"
LEFT JOIN go_fleet_config_attempts fa ON fa.vehicle_id = v."id"
WHERE length(v."vin") = 17
  AND NOT EXISTS (
        SELECT 1
        FROM go_removed_vehicles rv
        WHERE rv.user_id = v."userId"
          AND (rv.tesla_vehicle_id = v."teslaVehicleId" OR rv.vin = v."vin")
      )
  AND NOT EXISTS (` + unacknowledgedDriverAccessGate + `
      )
ORDER BY v."lastUpdated" DESC, v."id" ASC
LIMIT $1`

// ListStreamingFleetConfigVehicles returns up to limit cars for the MYR-630
// re-push sweep, most recently active first.
//
// A non-positive limit returns no rows and no error, on the same reading as
// ListFleetConfigCandidates: refusing to run an unbounded scan against the
// Prisma-owned table is the safer meaning of a zero.
func (r *VehicleRepo) ListStreamingFleetConfigVehicles(
	ctx context.Context, limit int,
) ([]StreamingFleetVehicle, error) {
	if limit <= 0 {
		return nil, nil
	}
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryStreamingFleetConfigVehicles, limit)
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_streaming_fleet_config_vehicles")
		return nil, fmt.Errorf("VehicleRepo.ListStreamingFleetConfigVehicles: %w", err)
	}
	defer rows.Close()

	out := make([]StreamingFleetVehicle, 0, limit)
	for rows.Next() {
		var v StreamingFleetVehicle
		if err := rows.Scan(&v.VehicleID, &v.VIN, &v.UserID, &v.VehicleName,
			&v.LastUpdated, &v.Status,
			&v.Suspended, &v.ConfigAbsent, &v.PendingOwnerAck); err != nil {
			r.metrics.IncQueryError("vehicle.list_streaming_fleet_config_vehicles")
			return nil, fmt.Errorf("VehicleRepo.ListStreamingFleetConfigVehicles: scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("vehicle.list_streaming_fleet_config_vehicles")
		return nil, fmt.Errorf("VehicleRepo.ListStreamingFleetConfigVehicles: rows: %w", err)
	}
	r.metrics.ObserveQueryDuration("vehicle.list_streaming_fleet_config_vehicles", time.Since(start).Seconds())
	return out, nil
}
