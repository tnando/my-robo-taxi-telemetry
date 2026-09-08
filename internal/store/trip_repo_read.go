package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The MYR-602 trips READ path: one trip, the caller's list, and the four
// decorations a trip card renders.
//
// EVERY READ RESOLVES THE CALLER'S ROLE IN THE SAME STATEMENT THAT READS THE
// ROW (tripRoleExpr), so there is no read-then-authorize window, and a caller
// who is neither owner nor live participant receives ErrTripNotFound rather
// than a denial. That is the whole 403-vs-404 rule for this surface: a trip the
// caller is not on must be indistinguishable from a trip that does not exist,
// or the endpoint is an oracle for trip ids.

// tripListLimitDefault and tripListLimitMax bound GET /api/trips.
//
// A person has a handful of trips, not a feed — which is why the contract's
// envelope is unpaginated — so the cap exists to bound the DECORATION work (a
// roster and a drive count per row), not the row count.
const (
	tripListLimitDefault = 50
	tripListLimitMax     = 100
)

// NormalizeTripListLimit clamps a requested limit into range. Exported so the
// handler and the repository cannot disagree about what `?limit=0` means.
func NormalizeTripListLimit(limit int) int {
	switch {
	case limit <= 0:
		return tripListLimitDefault
	case limit > tripListLimitMax:
		return tripListLimitMax
	default:
		return limit
	}
}

// GetForUser reads one trip, fully decorated, as the caller sees it.
//
// Returns ErrTripNotFound for an unknown id AND for a trip the caller has no
// relationship to. The two are the same answer deliberately.
func (r *TripRepo) GetForUser(ctx context.Context, tripID, userID string) (TripView, error) {
	const op = "trip.get"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	view, err := r.scanTripRow(r.pool.QueryRow(ctx, queryTripByIDForUser, userID, tripID))
	switch {
	case errors.Is(err, ErrTripNotFound):
		return TripView{}, ErrTripNotFound
	case err != nil:
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.GetForUser(%s): %w", tripID, err)
	}

	if err := r.decorate(ctx, &view); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.GetForUser(%s): %w", tripID, err)
	}
	return view, nil
}

// ListForUser returns the caller's trips — owned or live-participated — newest
// first, optionally narrowed to one derived status.
//
// status is the empty string for "all". It is compared in SQL rather than in Go
// so `limit` means "N trips of that status": filtering after the LIMIT would
// return a short page while more matching trips sat behind it.
func (r *TripRepo) ListForUser(ctx context.Context, userID string, status TripStatus, limit int) ([]TripView, error) {
	const op = "trip.list_for_user"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	rows, err := r.pool.Query(ctx, queryTripsForUser, userID, string(status), NormalizeTripListLimit(limit))
	if err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.ListForUser(%s): %w", userID, err)
	}
	defer rows.Close()

	views := make([]TripView, 0, 8)
	for rows.Next() {
		view, scanErr := r.scanTripRow(rows)
		if scanErr != nil {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.ListForUser(%s): %w", userID, scanErr)
		}
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.ListForUser(%s): rows: %w", userID, err)
	}
	// The cursor must be closed before the decoration queries run: they use the
	// same pool and a still-open Rows holds its connection.
	rows.Close()

	// DECORATED PER ROW rather than in four batched queries. The list is capped
	// at a hundred and a person has a handful of trips, so the batching would
	// buy microseconds and cost the property that a list row and a detail row
	// are built by the same code and therefore cannot disagree.
	for i := range views {
		if err := r.decorate(ctx, &views[i]); err != nil {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.ListForUser(%s): %w", userID, err)
		}
	}
	return views, nil
}

// scanTripRow reads the go_trips projection plus the resolved role, opening the
// sealed name.
//
// A NULL role is ErrTripNotFound. That translation happens HERE, at the scan,
// so no caller can receive a TripView with an empty Role and go on to use it.
func (r *TripRepo) scanTripRow(row pgx.Row) (TripView, error) {
	var (
		v       TripView
		nameEnc string
		role    *string
	)
	err := row.Scan(
		&v.ID, &v.VehicleID, &v.OwnerUserID, &nameEnc,
		&v.StartsAt, &v.EndsAt, &v.EndedAt,
		&v.StartedNotifiedAt, &v.EndedNotifiedAt,
		&v.CreatedAt, &v.UpdatedAt,
		&role,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return TripView{}, ErrTripNotFound
	case err != nil:
		return TripView{}, fmt.Errorf("scan trip: %w", err)
	}
	if role == nil || *role == "" {
		return TripView{}, ErrTripNotFound
	}
	v.Role = *role

	// FAIL-SOFT on the name, matching every other encrypted label in this
	// package: a row whose ciphertext will not open reports an empty name and
	// increments IncDecryptFailure rather than 500ing the whole list. A trip
	// the owner cannot see is worse than a trip with a blank title, and the
	// counter is what turns a wrong ENCRYPTION_KEY into a minutes-not-months
	// detection.
	v.Name = encStringToLabel(&nameEnc, r.encryptor, r.logger, r.metrics, "nameEnc")
	return v, nil
}

// decorate fills the four read-time decorations. Each is independent; a failure
// in any of them fails the read, because a trip card that silently rendered an
// empty roster would say something false about who can see the car.
func (r *TripRepo) decorate(ctx context.Context, v *TripView) error {
	if err := r.loadVehicle(ctx, v); err != nil {
		return err
	}
	if err := r.loadOwnerFirstName(ctx, v); err != nil {
		return err
	}
	if err := r.loadRoster(ctx, v); err != nil {
		return err
	}
	if err := r.loadDriveTotals(ctx, v); err != nil {
		return err
	}
	return r.loadCurrentLeg(ctx, v)
}

func (r *TripRepo) loadVehicle(ctx context.Context, v *TripView) error {
	err := r.pool.QueryRow(ctx, queryTripVehicle, v.ID).Scan(
		&v.Vehicle.VehicleID, &v.Vehicle.Name, &v.Vehicle.Model,
		&v.Vehicle.Year, &v.Vehicle.Color, &v.Vehicle.VIN,
		&v.Vehicle.TrimLabel, &v.Vehicle.Trim,
	)
	if err != nil {
		return fmt.Errorf("load trip vehicle: %w", err)
	}
	return nil
}

func (r *TripRepo) loadOwnerFirstName(ctx context.Context, v *TripView) error {
	var name *string
	if err := r.pool.QueryRow(ctx, queryTripOwnerFirstName, v.ID).Scan(&name); err != nil {
		return fmt.Errorf("load trip owner name: %w", err)
	}
	// Reduced to a first token by the SAME helper behind VehicleSummary.
	// ownerFirstName and RideRequest.requesterName, so the platform shortens a
	// name exactly one way and "no resolvable name" has one spelling (nil).
	v.OwnerFirstName = ownerFirstNameToken(name)
	return nil
}

func (r *TripRepo) loadRoster(ctx context.Context, v *TripView) error {
	rows, err := r.pool.Query(ctx, queryTripRoster, v.ID)
	if err != nil {
		return fmt.Errorf("load trip roster: %w", err)
	}
	defer rows.Close()

	roster := make([]TripParticipantView, 0, 4)
	for rows.Next() {
		var (
			p              TripParticipantView
			label          string
			acceptedByName *string
			addedByName    *string
		)
		if err := rows.Scan(&p.ParticipantID, &p.UserID, &label, &acceptedByName, &addedByName); err != nil {
			return fmt.Errorf("load trip roster: scan: %w", err)
		}
		// MYR-618's attribution, reduced by the SAME helper as every other name
		// on the platform so "Added by" and "ownerFirstName" cannot shorten one
		// person's name two different ways. Nil stays nil — there is no
		// fallback here, because there is nothing to fall back TO: a label
		// belongs to a grant, and the adder may not hold one (the owner does
		// not).
		p.AddedByName = ownerFirstNameToken(addedByName)
		// THE ROSTER RULE (MYR-581): the accepting account's CONFIRMED first
		// name wins; the owner's own label for the grant is the fallback. The
		// fallback matters — an owner who invited "Mom" before she had been
		// through the naming prompt should see "Mom", not a blank row.
		if first := ownerFirstNameToken(acceptedByName); first != nil {
			p.Name = *first
		} else {
			p.Name = label
		}
		roster = append(roster, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load trip roster: rows: %w", err)
	}
	v.Participants = roster
	return nil
}

// loadDriveTotals fills `driveCount`, `totalDistanceMiles`,
// `totalDurationSeconds` and `totalEnergyKwh` from ONE statement over ONE
// window (MYR-608, widened by MYR-629).
//
// THE THREE TRAVEL TOGETHER FOR TWO REASONS. The cheap one is cost: §7.30.2
// decorates every row it returns, so a separate SUM would have added a round
// trip PER TRIP to a list that already issues five, and this adds none. The
// load-bearing one is agreement — a count and a total read by two statements
// could straddle a drive being written and describe two different sets of
// drives on one card, and there is no way for a reader to tell.
//
// The three sums are NULLABLE and stay nullable: SUM over zero rows is NULL,
// and that is the honest spelling of "this window has no drives yet". The
// ENERGY sum is null under a second condition as well — any drive that moved
// and reported EXACTLY 0 voids the whole total — because its column is NOT NULL
// and a zero in it is an absence, not a measurement. A NEGATIVE row is a
// measurement (a net-regen leg) and sums like any other; the window total is
// floored at 0 once, after the legs are added up. See queryTripDriveTotals.
func (r *TripRepo) loadDriveTotals(ctx context.Context, v *TripView) error {
	w := v.Window()
	err := r.pool.QueryRow(ctx, queryTripDriveTotals, w.VehicleID, w.From, w.To).
		Scan(&v.DriveCount, &v.TotalDistanceMiles, &v.TotalDurationMinutes, &v.TotalEnergyKwh)
	if err != nil {
		return fmt.Errorf("load trip drive totals: %w", err)
	}
	return nil
}

// loadCurrentLeg reads the open leg, if any. READ-ONLY: the leg rows are
// written by the leg detector, and nothing in this repository creates, updates
// or ends one.
//
// Absent while the trip is not active, without asking the database: a leg left
// open past the end of a window (the detector had not run yet, or the process
// was down when the window closed) must not render as "currently driving to
// Barstow" on a trip whose card says ENDED. The window is the authority on both
// surfaces.
func (r *TripRepo) loadCurrentLeg(ctx context.Context, v *TripView) error {
	if v.StatusAt(time.Now()) != TripStatusActive {
		return nil
	}

	var (
		leg     TripLegView
		destEnc string
	)
	err := r.pool.QueryRow(ctx, queryTripOpenLeg, v.ID).Scan(&leg.StartedAt, &destEnc, &leg.EtaMinutes)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The ordinary overnight state, not a degradation.
		return nil
	case err != nil:
		return fmt.Errorf("load trip current leg: %w", err)
	}

	leg.DestinationName = encStringToLabel(&destEnc, r.encryptor, r.logger, r.metrics, "destinationNameEnc")
	v.CurrentLeg = &leg
	return nil
}
