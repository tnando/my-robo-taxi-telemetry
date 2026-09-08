package telemetry

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// VehicleFleetConfigHandler serves the vehicleId-keyed fleet-config routes
// GET/POST /api/fleet-config/vehicle/{vehicleId}.
//
// It exists because browser clients never receive a full VIN (P0-masked to
// last-4), so the bench UI keys the re-push + status by vehicleId. This
// handler resolves the VIN server-side via a VehicleSnapshotReader (where the
// full VIN never leaves the process) and delegates to the shared
// FleetConfigHandler cores, so the two keying paths stay in lockstep.
type VehicleFleetConfigHandler struct {
	core     *FleetConfigHandler
	vehicles VehicleSnapshotReader
	logger   *slog.Logger
}

// NewVehicleFleetConfigHandler wires the vehicleId-keyed handler on top of an
// existing (VIN-keyed) FleetConfigHandler and a vehicleId→VIN reader.
func NewVehicleFleetConfigHandler(
	core *FleetConfigHandler,
	vehicles VehicleSnapshotReader,
	logger *slog.Logger,
) *VehicleFleetConfigHandler {
	return &VehicleFleetConfigHandler{core: core, vehicles: vehicles, logger: logger}
}

// ServeHTTP routes GET (status) and POST (re-push).
func (h *VehicleFleetConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
		h.handle(w, r, r.Method == http.MethodPost)
	default:
		h.core.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
	}
}

// handle resolves vehicleId → (VIN, owner), enforces JWT + ownership +
// Tesla token, then delegates to the shared push/status core.
func (h *VehicleFleetConfigHandler) handle(w http.ResponseWriter, r *http.Request, push bool) {
	vehicleID := r.PathValue("vehicleId")
	if vehicleID == "" {
		h.core.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		h.core.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}

	ctx := r.Context()

	userID, err := h.core.auth.ValidateToken(ctx, token)
	if err != nil {
		// THE SHARED CLASSIFIER, like every sibling handler (MYR-612 review).
		// This was the ONE surface still hard-coding 401: an unanswerable
		// existence probe told a phone its credential was dead, and the app's
		// response to that is to discard the session. See wserrors.AuthFailure.
		h.logger.Warn("vehicle fleet config: invalid token", slog.String("error", err.Error()))
		status, code, message := wserrors.AuthFailure(err)
		h.core.writeError(w, status, code, message)
		return
	}

	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// 404 intentionally indistinguishable from ownership-filtered
			// (rest-api.md §4.1.1) — never leak vehicle existence.
			h.core.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return
		}
		h.logger.Error("vehicle fleet config: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.core.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	if row.UserID != userID {
		h.logger.Warn("vehicle fleet config: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.core.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return
	}

	// MYR-599: the consent gate, and it guards the PUSH only. A driver whose
	// acknowledgment is still outstanding may perfectly well READ the config
	// status of the car they linked — the GET makes no change to anybody's
	// property and answering it is how the client learns there is no config —
	// so the gate sits inside the `push` branch rather than above this line.
	//
	// 409, matching the reconnect endpoint's refusal for the same reason: the
	// caller is not forbidden and nothing failed; the request does not apply to
	// this vehicle yet, and §7.29 is the specific thing that changes that.
	if push && row.DriverAccess.PendingAcknowledgment() {
		h.logger.Info("vehicle fleet config: push refused, driver-access car awaiting the owner-approval acknowledgment",
			slog.String("event", "fleet_config_awaiting_owner_ack"),
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.core.writeError(w, http.StatusConflict, wserrors.ErrCodeInvalidRequest,
			"confirm the owner approved adding this car before it can be configured")
		return
	}

	teslaTok, ok := h.core.resolveTeslaToken(ctx, w, userID)
	if !ok {
		return
	}

	if push {
		// pushGatedVIN, not pushForVIN: the consent gate was settled above from
		// the joined row this handler already holds. Going through pushForVIN
		// would spend a second query re-deriving it, from a read taken at a
		// different instant than the one the refusal above used.
		h.core.pushGatedVIN(ctx, w, row.VIN, teslaTok)
		return
	}
	h.core.writeStatusForVIN(ctx, w, row.VIN, teslaTok)
}
