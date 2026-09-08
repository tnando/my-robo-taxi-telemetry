package telemetry

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// THE OWNER GATE every vehicle-scoped sharing route passes through: prove the
// bearer, resolve the path vehicle, prove the caller owns it — and, for the
// routes that GRANT access, prove the §7.29 owner-approval acknowledgment has
// been made.
//
// Split out of share_invite_handler.go by MYR-609 under the 300-line rule, and
// the seam is the one the file already had: five routes share these two
// functions and nothing else on the handler is shared by all of them. MYR-609
// is also what made both of them multi-caller rather than create-private, which
// is why `surface` is a parameter on each.

// authOwner validates the bearer token and confirms the caller OWNS the path
// vehicle, writing the appropriate error response on failure.
//
// An unknown vehicle is 404 and a known-but-not-yours vehicle is 403, matching
// the rest of the per-vehicle surface. There is no viewer branch: a viewer's
// vehicle read succeeds, so they reach the ownership check and are refused
// exactly as an unrelated caller is.
// It returns the ROW as well as the ids, because the MYR-599 consent gate is a
// fact about that row and re-fetching it in the caller would mean two reads at
// two instants answering one question.
func (h *ShareInviteHandler) authOwner(
	w http.ResponseWriter, r *http.Request, surface string,
) (row VehicleSnapshotRow, vehicleID, userID string, ok bool) {
	vehicleID = r.PathValue("vehicleId")
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return VehicleSnapshotRow{}, "", "", false
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return VehicleSnapshotRow{}, "", "", false
	}

	ctx := r.Context()
	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn(surface+": invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return VehicleSnapshotRow{}, "", "", false
	}

	row, err = h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return VehicleSnapshotRow{}, "", "", false
		}
		h.logger.Error(surface+": vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return VehicleSnapshotRow{}, "", "", false
	}
	if row.UserID != userID {
		h.logger.Warn(surface+": not the owner",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return VehicleSnapshotRow{}, "", "", false
	}
	return row, vehicleID, userID, true
}

// driverAccessAllows is the MYR-599 consent gate (review finding I). Returns
// false having already written the response.
//
// IT GUARDS THE ROUTES THAT GRANT ACCESS — §7.5.1 create and, since MYR-609,
// §7.5.8 extend — AND NOT THE READS, and the split is the same one the
// fleet-config surface makes. Sharing a car is an act of DISPOSAL over somebody
// else's property — it hands a third party standing access to a vehicle whose owner
// has not yet been recorded as agreeing the car belongs here at all — while
// LISTING the invites on it changes nothing and is how a client renders the
// screen it is refusing from. A driver whose acknowledgment is outstanding has
// no invites to list anyway; refusing the read would only make the screen
// wrong.
//
// 409, matching the reconnect, fleet-config and command refusals: nothing
// failed, the caller is not forbidden, and §7.29 is the specific thing that
// changes the answer.
//
// `surface` NAMES THE ROUTE THAT WAS REFUSED, and it is a parameter rather than
// a constant because MYR-609 gave the gate a SECOND caller. A hard-coded
// "create" would have made every extend refusal log a line about a route the
// caller never called — cosmetic on one line, and wrong on the grep an operator
// runs to count how often each surface is being blocked.
//
// It rides in an ATTRIBUTE, not in the message. The message is a CONSTANT, so
// the refusal is one greppable string and one countable class however many
// routes the gate ends up guarding; the surface is the field an operator groups
// by. Interpolating it into the message would have produced N distinct log
// lines for one condition and made the aggregate the harder query — which is
// the reverse of what a structured logger is for. The `event` slug is unchanged
// and stays the stable key.
func (h *ShareInviteHandler) driverAccessAllows(
	w http.ResponseWriter, row VehicleSnapshotRow, vehicleID, userID, surface string,
) bool {
	if !row.DriverAccess.PendingAcknowledgment() {
		return true
	}
	h.logger.Info("share surface refused: driver-access car awaiting the owner-approval acknowledgment",
		slog.String("event", "share_invite_awaiting_owner_ack"),
		slog.String("surface", surface),
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
	)
	h.writeError(w, http.StatusConflict, wserrors.ErrCodeInvalidRequest,
		"confirm the owner approved adding this car before you can share it")
	return false
}
