package auth

import (
	"context"
	"errors"
	"testing"
)

// MYR-602 — TRIP-WINDOW PARTICIPATION as the fourth source of vehicle access,
// and the reversal of the precedence order that came with it.
//
// The SQL half (the access-set UNION leg, the window predicate and the share
// re-join) is pinned by the store's integration tests. What is pinned here is
// the resolution: which role a participant gets, what happens to the grant they
// also hold, and which way each failure resolves.

// stubTripParticipation answers the per-vehicle trip-window probe.
type stubTripParticipation struct {
	participant bool
	err         error
	calls       int
}

func (s *stubTripParticipation) IsActiveTripParticipant(context.Context, string, string) (bool, error) {
	s.calls++
	return s.participant, s.err
}

func TestResolveVehicleAccess_TripParticipation(t *testing.T) {
	const (
		caller  = "cjoiner-602"
		owner   = "cowner-602"
		vehicle = "cveh-602"
	)

	tests := []struct {
		name        string
		grants      map[string]ShareGrant
		noTripDep   bool
		participant bool
		tripErr     error
		riding      bool
		wantRole    Role
		wantRides   bool
		wantDenied  bool
		wantProbes  int
	}{
		{
			// THE ADMISSION THIS ISSUE EXISTS FOR. Every participant is by
			// construction also a share-holder, so this is the shape every
			// real resolution takes: a live accepted share PLUS an open
			// window, resolving to the elevated role rather than to viewer.
			name:        "a share-holder inside an open window is a trip participant",
			grants:      map[string]ShareGrant{caller + "|" + vehicle: {}},
			participant: true,
			wantRole:    RoleTripParticipant,
			wantProbes:  1,
		},
		{
			// THE CAPABILITY IS CARRIED THROUGH, not replaced. The trip
			// elevates the ROLE; it does not restate the relationship. A
			// participant who also holds a `rides` share can still summon the
			// car during the window, exactly as they could outside it.
			name:        "the elevated role keeps the grant the participant actually holds",
			grants:      map[string]ShareGrant{caller + "|" + vehicle: {AllowRides: true}},
			participant: true,
			wantRole:    RoleTripParticipant,
			wantRides:   true,
			wantProbes:  1,
		},
		{
			// THE WINDOW CLOSED. Nothing was revoked and no row changed — the
			// clock simply passed the end instant — and the caller falls back
			// to the share they still hold, now narrowed.
			name:       "outside the window the same person is an ordinary viewer",
			grants:     map[string]ShareGrant{caller + "|" + vehicle: {}},
			wantRole:   RoleViewer,
			wantProbes: 1,
		},
		{
			// TRIP BEATS RIDE when both are true. The two roles carry the same
			// field set, so what the order decides is the PROVENANCE the
			// handlers read — and trip participation is the more specific of
			// the two, because it is what admits the caller to the window's
			// drives.
			name:        "trip participation wins over ride membership",
			grants:      map[string]ShareGrant{caller + "|" + vehicle: {}},
			participant: true,
			riding:      true,
			wantRole:    RoleTripParticipant,
			wantProbes:  1,
		},
		{
			// THE HOLE THIS TEST EXISTS TO KEEP CLOSED. ShareGrant's zero value
			// has Suspended=false, so ShareGrant{}.Active() is TRUE: an
			// implementation that folded "no grant" into the zero value and
			// then asked Active() would admit EVERY AUTHENTICATED STRANGER as a
			// viewer on EVERY vehicle. The share lookup's existence flag is
			// what denies here, not the grant's contents.
			name:       "a stranger with no share and no window is denied",
			wantDenied: true,
			wantProbes: 1,
		},
		{
			// FAILS CLOSED as `false`, not as an error: the probe runs on a
			// path that has a correct answer without it (the share tier, or a
			// denial), so a database blip must not turn a request into a 500.
			name:       "an unreadable trip probe falls back to the narrower role",
			grants:     map[string]ShareGrant{caller + "|" + vehicle: {}},
			tripErr:    errors.New("boom"),
			wantRole:   RoleViewer,
			wantProbes: 1,
		},
		{
			// Unwired is the pre-MYR-602 behaviour, unchanged.
			name:       "no trip lookup configured grants nothing",
			grants:     map[string]ShareGrant{caller + "|" + vehicle: {}},
			noTripDep:  true,
			wantRole:   RoleViewer,
			wantProbes: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			querier := &stubQuerier{ownerByID: map[string]string{vehicle: owner}}
			trips := &stubTripParticipation{participant: tt.participant, err: tt.tripErr}
			a := &JWTAuthenticator{
				secret:      []byte(testSecret),
				cache:       newVehicleCache(querier, vehicleCacheTTL),
				ownerLookup: querier,
				shares:      &stubShareLookup{grants: tt.grants},
				rides:       &stubRideMembership{riding: tt.riding},
			}
			if !tt.noTripDep {
				a.trips = trips
			}

			role, grant, err := a.ResolveVehicleAccess(context.Background(), caller, vehicle)

			if tt.wantDenied {
				if !errors.Is(err, ErrNoVehicleAccess) {
					t.Fatalf("err = %v, want ErrNoVehicleAccess", err)
				}
				if role != Role("") {
					t.Errorf("role = %q, want the empty deny-all sentinel", role)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if role != tt.wantRole {
					t.Errorf("role = %q, want %q", role, tt.wantRole)
				}
				if grant.GrantsRides() != tt.wantRides {
					t.Errorf("GrantsRides() = %v, want %v", grant.GrantsRides(), tt.wantRides)
				}
			}
			if trips.calls != tt.wantProbes {
				t.Errorf("trip probe ran %d time(s), want %d", trips.calls, tt.wantProbes)
			}
		})
	}
}

// TestResolveVehicleAccess_SuspendedShareStillDeniesInsideAWindow is the
// narrowest and most important consequence of the precedence reversal.
//
// The elevated probes run BEFORE the suspension gate, so if the trip probe were
// allowed to answer for somebody whose share is suspended, a revoked or
// suspended grant would be resurrected by an open window. It cannot be: the
// access query re-joins `status = 'accepted' AND suspended_at IS NULL` on every
// resolution, so the probe itself returns false. This test pins the Go-side half
// — that a suspended grant with the probe saying "no" is a denial and not a
// viewer with an empty capability set (MYR-369).
func TestResolveVehicleAccess_SuspendedShareStillDeniesInsideAWindow(t *testing.T) {
	const caller, owner, vehicle = "cjoiner-602", "cowner-602", "cveh-602"

	querier := &stubQuerier{ownerByID: map[string]string{vehicle: owner}}
	a := &JWTAuthenticator{
		secret:      []byte(testSecret),
		cache:       newVehicleCache(querier, vehicleCacheTTL),
		ownerLookup: querier,
		shares: &stubShareLookup{grants: map[string]ShareGrant{
			caller + "|" + vehicle: {Suspended: true},
		}},
		// The DB-backed probe cannot say true here — its share join excludes
		// suspended rows — so this stub is deliberately the FAITHFUL one.
		trips: &stubTripParticipation{participant: false},
	}

	role, _, err := a.ResolveVehicleAccess(context.Background(), caller, vehicle)
	if !errors.Is(err, ErrNoVehicleAccess) {
		t.Fatalf("err = %v, want ErrNoVehicleAccess", err)
	}
	if role != Role("") {
		t.Errorf("role = %q, want the empty deny-all sentinel", role)
	}
}
