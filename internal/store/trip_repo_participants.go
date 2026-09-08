package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MYR-618: THE ROSTER, WIDENED TO EVERYBODY ON THE TRIP.
//
// Until now one person could change a trip's roster — the owner — and every
// statement that wrote go_trip_participants was reached through an owner-scoped
// gate. This file adds the second writer:
//
//	AddParticipants  the owner OR a live participant admits people to a trip
//
// The READ that feeds its picker — who those people can be (§7.30.11) — is in
// trip_repo_addable_people.go. Same feature, opposite direction: this file is a
// transaction that changes a roster, that one is a projection that can grant
// nothing.
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

	if err := addAndAuditParticipants(ctx, tx, tripID, access.VehicleID, actorUserID,
		access.Role == tripRoleOwner, shareIDs); err != nil {
		if !errors.Is(err, ErrTripParticipantNotShared) && !errors.Is(err, ErrTripParticipantOwnerRemoved) {
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
	ctx context.Context, tx tripQuerier, tripID, vehicleID, actorUserID string,
	actorIsOwner bool, shareIDs []string,
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

	if err := refuseOwnerRemoved(ctx, tx, tripID, actorIsOwner, resolved); err != nil {
		if errors.Is(err, ErrTripParticipantOwnerRemoved) {
			return err
		}
		return fmt.Errorf("add participants(trip=%s): %w", tripID, err)
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

// refuseOwnerRemoved is the review round's gate: A PARTICIPANT MAY NOT UNDO AN
// OWNER'S REMOVE (migration 0061).
//
// ── WHY THE ROSTER NEEDED THIS ──────────────────────────────────────────────
//
// The upsert REVIVES a departed membership, which is right for somebody who
// left and wrong for somebody the owner took off. Without this gate the owner's
// remove — the one roster verb MYR-618 deliberately kept owner-only — would
// have been the one verb any participant could reverse, immediately and as
// often as they liked, by re-sending the same share id.
//
// ── IT RUNS BEFORE THE UPSERT, NOT AS A CONSTRAINT ON IT ────────────────────
//
// The refusal has to be ALL-OR-NOTHING across the request, the way
// `participant_not_shared` is: a request naming three people, one of whom the
// owner removed, must add none of them rather than two. A predicate inside the
// upsert would silently skip the row instead, and the caller would get a 200
// carrying a roster missing somebody they asked for.
//
// ── AN OWNER SKIPS IT ENTIRELY ──────────────────────────────────────────────
//
// Not "an owner passes it" — the statement is not issued at all. An owner's add
// is the act that CLEARS the marker (see queryUpsertTripParticipant), so asking
// the question would be asking whether they may undo their own decision.
//
// ── DELIBERATELY UNSPECIFIC ABOUT WHO ───────────────────────────────────────
//
// The error names nobody, exactly as `participant_not_shared` names nobody:
// which of several people the owner removed is a fact about the owner's own
// decisions, and the client's next move is the same either way — tell the
// person to ask the owner.
func refuseOwnerRemoved(
	ctx context.Context, q tripQuerier, tripID string, actorIsOwner bool, resolved []TripParticipantView,
) error {
	if actorIsOwner || len(resolved) == 0 {
		return nil
	}
	userIDs := make([]string, 0, len(resolved))
	for _, p := range resolved {
		userIDs = append(userIDs, p.UserID)
	}

	rows, err := q.Query(ctx, queryTripOwnerRemovedUserIDs, tripID, userIDs)
	if err != nil {
		return fmt.Errorf("read owner-removed roster: %w", err)
	}
	defer rows.Close()

	removed := false
	for rows.Next() {
		removed = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read owner-removed roster: rows: %w", err)
	}
	if removed {
		return ErrTripParticipantOwnerRemoved
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
// metadata: TWO OPAQUE CUIDS AND NOTHING ELSE IN THE `metadata` COLUMN, which
// is THREE across the whole row once `targetId` (the trip) is counted — that is
// the number rest-api.md §7.30.4 and data-lifecycle.md §4.2 quote, and the two
// figures differ only in scope. P0 throughout (CG-DL-5).
//
// The SHARE id rather than the added person's user id, deliberately. The share
// already names them to anybody entitled to ask this question — it is a row on
// the car's own grant list — while a user id would be a durable cross-surface
// identifier for a third party who is not the subject of this row. No trip
// name (P1 user content, sealed at rest) and no names of any kind.
//
// The `userId` column is the ACTOR and is not counted among the three: it is
// who the row is FILED UNDER, not something the row discloses.
type tripParticipantAddedAuditMetadata struct {
	VehicleID string `json:"vehicleId"`
	ShareID   string `json:"shareId"`
}

// insertTripParticipantAddedAudit writes one `trip.participant_added` row
// inside the roster transaction, through the shared writer in trip_audit.go.
//
// `userId` IS THE ACTOR, and that is the entire point of the row: since MYR-618
// the person who widened a roster is no longer necessarily its owner.
func insertTripParticipantAddedAudit(
	ctx context.Context, tx tripQuerier, actorUserID, tripID, vehicleID, shareID string,
) error {
	return insertTripAudit(ctx, tx, AuditActionTripParticipantAdded, actorUserID, tripID,
		tripParticipantAddedAuditMetadata{VehicleID: vehicleID, ShareID: shareID})
}
