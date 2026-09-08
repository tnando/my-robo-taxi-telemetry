package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// MYR-618: THE ROSTER, WIDENED TO EVERYBODY ON THE TRIP.
//
// Until now one person could change a trip's roster — the owner — and every
// statement that wrote go_trip_participants was reached through an owner-scoped
// gate. This file adds the second writer and the read that feeds it:
//
//	AddParticipants  the owner OR a live participant admits people to a trip
//	AddablePeople    who those people can be (§7.30.11)
//
// ⚠ THE RULE THAT MAKES THIS SAFE IS THAT A TRIP MINTS NOTHING. A participant
// can only name somebody who ALREADY holds an accepted, unsuspended grant on
// the car — the same predicate the owner's own add uses, unchanged — so the
// widest thing a participant can do is move a person the owner already trusted
// with the vehicle from "can see it whenever I share it" to "can see it during
// this window". They cannot create a grant, cannot lengthen a window, cannot
// remove anybody and cannot end anything. Every one of those stays owner-only,
// enforced in the handler by refusing the whole request and here by the fact
// that this file contains no statement that could do any of them.

// tripAccessRow is the cheap role probe's result: the caller's relationship to
// a trip plus the three columns that decide whether the trip is still live.
//
// Deliberately NOT a TripView. The full read decrypts a name, resolves a trim,
// counts drives and reads a leg — five round trips to answer a question that is
// "may this person act on this trip, and is it over?". A gate that costs that
// much is a gate somebody eventually caches.
type tripAccessRow struct {
	VehicleID   string
	OwnerUserID string
	StartsAt    time.Time
	EndsAt      time.Time
	EndedAt     *time.Time
	Role        string
}

// ended reports whether the window has closed, by the SAME rule Trip.StatusAt
// applies: a stamped `ended_at` is terminal on its own, and otherwise the
// scheduled end decides.
func (r tripAccessRow) ended(now time.Time) bool {
	if r.EndedAt != nil {
		return true
	}
	return !now.Before(r.EndsAt)
}

// tripAccessFor resolves the caller's role on one trip, or ErrTripNotFound.
//
// ONE ANSWER FOR "NO SUCH TRIP" AND "NOT YOUR TRIP", the 404-not-403 rule this
// surface is built on, applied through the same `tripRoleExpr` every other read
// uses rather than through a second predicate that could drift from it.
func tripAccessFor(ctx context.Context, q tripQuerier, tripID, userID string) (tripAccessRow, error) {
	var (
		row  tripAccessRow
		role *string
	)
	err := q.QueryRow(ctx, queryTripRoleForUser, userID, tripID).Scan(
		&row.VehicleID, &row.OwnerUserID, &row.StartsAt, &row.EndsAt, &row.EndedAt, &role,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return tripAccessRow{}, ErrTripNotFound
	case err != nil:
		return tripAccessRow{}, fmt.Errorf("trip access probe: %w", err)
	}
	if role == nil || *role == "" {
		return tripAccessRow{}, ErrTripNotFound
	}
	row.Role = *role
	return row, nil
}

// AddParticipants admits people to an existing trip on behalf of ANY live
// member — the owner or a participant (MYR-618, §7.30.4).
//
// It is a SEPARATE ENTRY POINT from Update rather than a mode of it, and the
// separation is the security argument rather than a style choice: Update's
// first act is `loadOwnedTripForPatch`, which refuses everybody but the owner,
// and every statement it issues afterwards is owner-scoped. Threading a
// "sometimes the caller is a participant" flag through that would put the
// widest gate on this surface behind a boolean. This function can only ever
// widen a roster, because widening a roster is the only statement it contains.
//
// ONE TRANSACTION: the resolve, the roster read, the upserts and the audit rows
// commit together. An audit row without its membership would be a record of an
// act that did not happen, and a membership without its audit row is the state
// nobody could later explain — which is the pair CG-DL-3 exists to keep whole.
//
// REFUSES ON AN ENDED TRIP (ErrTripEnded), exactly as the owner's patch does
// and for the same reason: adding somebody to a window that has closed grants
// nothing, so the honest answer is that the trip is over rather than a 200 that
// appears to have worked.
func (r *TripRepo) AddParticipants(ctx context.Context, tripID, actorUserID string, shareIDs []string) (TripView, error) {
	const op = "trip.add_participants"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.AddParticipants(%s): begin: %w", tripID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	access, err := tripAccessFor(ctx, tx, tripID, actorUserID)
	if err != nil {
		if !errors.Is(err, ErrTripNotFound) {
			r.metrics.IncQueryError(op)
		}
		return TripView{}, err
	}
	if access.ended(time.Now()) {
		return TripView{}, ErrTripEnded
	}

	if err := addAndAuditParticipants(ctx, tx, tripID, access.VehicleID, actorUserID, shareIDs); err != nil {
		if !errors.Is(err, ErrTripParticipantNotShared) {
			r.metrics.IncQueryError(op)
		}
		return TripView{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.AddParticipants(%s): commit: %w", tripID, err)
	}
	// Read back through the ONE view builder, as create and patch do, so the
	// response to an add and a subsequent GET cannot disagree about the roster.
	return r.GetForUser(ctx, tripID, actorUserID)
}

// addAndAuditParticipants is the shared body of "somebody was added to a trip
// that already existed": resolve, diff against the live roster, upsert, audit.
//
// THE AUDIT ROWS ARE THE DIFF. A re-send of somebody already on the trip
// resolves fine, upserts to a no-op and appears in no audit row — which is what
// "already-present → no-op 200" means one layer up. The set is deliberately NOT
// returned: the handler computes its own before/after roster diff for the push
// fan-out (it has both TripViews already), and a second answer to the same
// question, produced here and passed up through two adapters, is a second
// answer that could disagree.
//
// THE ROSTER IS READ INSIDE THE TRANSACTION and once. It decides both the audit
// rows and the caller's fan-out list; reading it twice, or outside the
// transaction, would let a concurrent leave land between the two and produce an
// audit row for an add nobody was told about.
func addAndAuditParticipants(
	ctx context.Context, tx tripQuerier, tripID, vehicleID, actorUserID string, shareIDs []string,
) error {
	resolved, err := resolveShareParticipants(ctx, tx, vehicleID, shareIDs)
	if err != nil {
		if errors.Is(err, ErrTripParticipantNotShared) {
			return err
		}
		return fmt.Errorf("add participants(trip=%s): %w", tripID, err)
	}
	if len(resolved) == 0 {
		return nil
	}

	present, err := liveParticipantUserIDs(ctx, tx, tripID)
	if err != nil {
		return fmt.Errorf("add participants(trip=%s): %w", tripID, err)
	}

	if err := addTripParticipants(ctx, tx, tripID, actorUserID, resolved); err != nil {
		return fmt.Errorf("add participants(trip=%s): %w", tripID, err)
	}

	for _, p := range resolved {
		if present[p.UserID] {
			continue
		}
		if err := insertTripParticipantAddedAudit(ctx, tx, actorUserID, tripID, vehicleID, p.ParticipantID); err != nil {
			return fmt.Errorf("add participants(trip=%s): %w", tripID, err)
		}
	}
	return nil
}

// liveParticipantUserIDs reads the trip's current roster as a set.
func liveParticipantUserIDs(ctx context.Context, q tripQuerier, tripID string) (map[string]bool, error) {
	rows, err := q.Query(ctx, queryTripLiveParticipantUserIDs, tripID)
	if err != nil {
		return nil, fmt.Errorf("read live roster: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool, 4)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("read live roster: scan: %w", err)
		}
		out[userID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read live roster: rows: %w", err)
	}
	return out, nil
}

// tripParticipantAddedAuditMetadata is the `trip.participant_added` row's
// metadata: TWO OPAQUE CUIDS AND NOTHING ELSE (CG-DL-5, P0-only).
//
// The SHARE id rather than the added person's user id, deliberately. The share
// already names them to anybody entitled to ask this question — it is a row on
// the car's own grant list — while a user id would be a durable cross-surface
// identifier for a third party who is not the subject of this row. No trip
// name (P1 user content, sealed at rest) and no names of any kind.
type tripParticipantAddedAuditMetadata struct {
	VehicleID string `json:"vehicleId"`
	ShareID   string `json:"shareId"`
}

// insertTripParticipantAddedAudit writes one `trip.participant_added` row inside
// the roster transaction, reusing the same-package queryAuditInsert column list
// (the single source of truth AuditRepo writes through — which is what keeps
// CG-DL-8 column parity automatic).
//
// `userId` IS THE ACTOR, and that is the entire point of the row: since MYR-618
// the person who widened a roster is no longer necessarily its owner.
func insertTripParticipantAddedAudit(
	ctx context.Context, tx tripQuerier, actorUserID, tripID, vehicleID, shareID string,
) error {
	meta, err := json.Marshal(tripParticipantAddedAuditMetadata{VehicleID: vehicleID, ShareID: shareID})
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(),                        // id (cuid)
		actorUserID,                             // userId — the OWNER or the PARTICIPANT who added
		now,                                     // timestamp
		string(AuditActionTripParticipantAdded), // action
		auditTargetTypeTrip,                     // targetType
		tripID,                                  // targetId
		auditInitiatorUser,                      // initiator
		meta,                                    // metadata (two opaque cuids)
		now,                                     // createdAt
	); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}

// AddablePeople answers §7.30.11: the vehicle's live grant-holders who are not
// already on this trip, for a caller who owns the trip or is on it.
//
// TWO STATEMENTS, AND THE FIRST ONE IS THE GATE. The list statement returns no
// rows for a stranger anyway, but an empty list is also the honest answer for a
// trip where everybody is already aboard — so without the role probe the two
// would be indistinguishable, and the endpoint would answer 200 to somebody who
// must receive the same 404 every other per-trip route gives them.
func (r *TripRepo) AddablePeople(ctx context.Context, tripID, userID string) ([]TripAddablePersonView, error) {
	const op = "trip.addable_people"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	if _, err := tripAccessFor(ctx, r.pool, tripID, userID); err != nil {
		if !errors.Is(err, ErrTripNotFound) {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.AddablePeople(%s): %w", tripID, err)
		}
		return nil, ErrTripNotFound
	}

	rows, err := r.pool.Query(ctx, queryTripAddablePeople, tripID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.AddablePeople(%s): %w", tripID, err)
	}
	defer rows.Close()

	out := make([]TripAddablePersonView, 0, 8)
	for rows.Next() {
		var (
			person         TripAddablePersonView
			label          string
			acceptedByName *string
		)
		if err := rows.Scan(&person.ShareID, &label, &acceptedByName); err != nil {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.AddablePeople(%s): scan: %w", tripID, err)
		}
		// THE SAME NAME RULE THE ROSTER USES (MYR-581/583), applied by the same
		// helper: the accepting account's confirmed first name wins, the
		// owner's own label for the grant is the fallback. A person must not be
		// called one thing in the picker and another thing on the roster one
		// tap later.
		person.DisplayName = label
		if first := ownerFirstNameToken(acceptedByName); first != nil {
			person.DisplayName = *first
		}
		out = append(out, person)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.AddablePeople(%s): rows: %w", tripID, err)
	}

	// Ordered for the reader, here rather than in SQL — see the statement's own
	// comment. The share id is the tie-break, so the order is TOTAL and a picker
	// showing two people the owner labelled the same does not reshuffle them
	// between reads.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].ShareID < out[j].ShareID
	})
	return out, nil
}
