package main

import (
	"context"
	"errors"
	"testing"
)

type stubDriverGate struct {
	pending bool
	err     error
	calls   int
}

func (g *stubDriverGate) PendingDriverAcknowledgmentByVIN(context.Context, string) (bool, error) {
	g.calls++
	return g.pending, g.err
}

// MYR-599 REVIEW FINDING I: `ops fleet-config push` WAS THE LAST UNGATED PUSH.
//
// It resolves its vehicle through GetByVIN, which does not join the
// driver-access table, so it could not have asked the question — and it is the
// one caller whose push is unconditional, unattended by any client state, and
// issued with the owner's own Tesla credentials. `ops vehicles list` printing
// `driver(unacknowledged)` made the state visible, which is a real answer for a
// warning and a poor one for an invariant: visibility that depends on somebody
// remembering to look is not a gate.
func TestFleetConfigPushRefusesAnUnacknowledgedDriverCar(t *testing.T) {
	tests := []struct {
		name    string
		gate    *stubDriverGate
		force   bool
		wantErr error
		because string
	}{
		{
			name: "a settled car proceeds",
			gate: &stubDriverGate{},
		},
		{
			name:    "an unacknowledged driver car is refused",
			gate:    &stubDriverGate{pending: true},
			wantErr: errUnacknowledgedDriverCar,
			because: "nothing has ever been configured for this car and its Tesla owner is not " +
				"on record as having agreed it belongs here",
		},
		{
			name:  "--force-unacknowledged overrides it",
			gate:  &stubDriverGate{pending: true},
			force: true,
			because: "the operator surface exists for the case the product did not anticipate, " +
				"and a support engineer with the owner on the phone may hold evidence this " +
				"database does not",
		},
		{
			name:    "a gate that cannot be read refuses",
			gate:    &stubDriverGate{err: errors.New("db down")},
			wantErr: errUnacknowledgedDriverCar, // sentinel unused; checked as non-nil below
			because: "an operator who cannot be told whether they may act has not been told " +
				"they may",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseUnacknowledgedDriverCar(
				context.Background(), tt.gate, "5YJ3E1EA1PF000001", tt.force)

			switch {
			case tt.gate.err != nil:
				if err == nil {
					t.Fatalf("lookup failure produced no error — %s", tt.because)
				}
				if errors.Is(err, errUnacknowledgedDriverCar) {
					t.Error("a lookup failure was reported as the consent refusal; the two mean " +
						"opposite things about what the operator should do next")
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want the consent refusal (%s)", err, tt.because)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v, want nil (%s)", err, tt.because)
				}
			}

			// THE OVERRIDE MUST NOT SKIP THE LOOKUP. An operator who overrides
			// the gate should see exactly what they overrode, and a session
			// audit should show the condition was real.
			if tt.gate.calls != 1 {
				t.Errorf("gate calls = %d, want 1", tt.gate.calls)
			}
		})
	}
}
