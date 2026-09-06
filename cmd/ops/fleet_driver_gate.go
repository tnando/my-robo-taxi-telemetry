package main

// The MYR-599 consent gate on `ops fleet-config push` (review finding I).
//
// ── WHY THE OPERATOR SURFACE NEEDED ONE TOO ──────────────────────────────────
//
// Every other push path in the server refuses a driver-linked car whose
// owner-approval acknowledgment is outstanding: the reconciler at its consumer,
// complete-setup at step zero, reconnect, both fleet-config routes, and now the
// command and share surfaces. This one resolved its vehicle through
// `GetByVIN`, which does not join the driver-access table, so it could not have
// asked the question even if it had wanted to — and it is the one caller whose
// push is unconditional, unattended by any client-side state, and issued with
// the owner's own Tesla credentials.
//
// `ops vehicles list` already PRINTS `driver(unacknowledged)`, and the previous
// round of this work left it at that: the state is visible, so the operator can
// look. That is a real answer for a warning and a poor one for an invariant.
// Visibility that depends on someone remembering to look is not a gate, and the
// thing on the other side of this one is a stranger's car.
//
// ── WHY IT IS AN OVERRIDE RATHER THAN A PROHIBITION ──────────────────────────
//
// Because the operator surface exists for the case the product did not
// anticipate, and a support engineer with the owner on the phone is exactly the
// person who might legitimately have the evidence this database does not. So
// the flag is offered — but it is spelled out in full, it is never implied by
// any other flag, and taking it prints the refusal it is overriding, so the
// override is a decision somebody made rather than a default they inherited.

import (
	"context"
	"fmt"
	"os"
)

// driverAccessGateReader is the one question this file asks. Consumer-site
// interface so the refusal can be tested without a database.
type driverAccessGateReader interface {
	PendingDriverAcknowledgmentByVIN(ctx context.Context, vin string) (bool, error)
}

// errUnacknowledgedDriverCar is what a refused push returns. A distinct value
// so a caller (and a test) can tell the refusal from a lookup failure — the two
// mean opposite things about what the operator should do next.
var errUnacknowledgedDriverCar = fmt.Errorf(
	"vehicle is driver-linked and its owner-approval acknowledgment is outstanding: " +
		"nothing has ever been configured for this car, and pushing would act on a vehicle " +
		"whose Tesla owner is not on record as having agreed. The person who linked it can " +
		"clear this from the app. Re-run with --force-unacknowledged only if you have the " +
		"owner's approval by another route")

// refuseUnacknowledgedDriverCar answers whether `ops fleet-config push` may
// proceed for this VIN.
//
// FAIL CLOSED ON THE LOOKUP ERROR, like every other copy of this gate. An
// operator who cannot be told whether they may act has not been told they may.
//
// force does NOT skip the lookup. The answer is printed either way, on stderr
// so it cannot be mistaken for part of the command's JSON output: an operator
// who overrides the gate should see exactly what they overrode, and an audit of
// the session should show that the condition was real.
//
// The VIN is printed unredacted, which is this command's existing convention
// rather than a new exemption: the operator typed it on the command line and
// `fleetPushOutput` echoes it back in full on stdout. Redacting it here alone
// would obscure which car was overridden while changing nothing about who can
// see it.
func refuseUnacknowledgedDriverCar(
	ctx context.Context, gate driverAccessGateReader, vin string, force bool,
) error {
	pending, err := gate.PendingDriverAcknowledgmentByVIN(ctx, vin)
	if err != nil {
		return fmt.Errorf("read driver-access gate: %w", err)
	}
	if !pending {
		return nil
	}
	if force {
		fmt.Fprintf(os.Stderr,
			"WARNING: --force-unacknowledged: pushing config for %s, a driver-linked vehicle "+
				"whose owner-approval acknowledgment is outstanding.\n", vin)
		return nil
	}
	return errUnacknowledgedDriverCar
}
