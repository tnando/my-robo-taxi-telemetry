package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// DriveRouteData is the handler-layer view of a single drive's recorded
// route, decoupled from internal/store (the cmd adapter maps a
// store.DriveRecord into it). RoutePoints is the already-decrypted JSONB
// breadcrumb array — the store's GetByID resolves the routePointsEnc
// shadow before the handler ever sees it.
type DriveRouteData struct {
	DriveID   string
	VehicleID string
	// StartTime is the drive's RFC 3339 start instant. Carried on this shape
	// since MYR-602 purely as an INPUT TO THE ACCESS CHECK — a trip
	// participant is admitted only to drives that began inside one of their
	// windows, and this endpoint's own response has never carried it. Not on
	// the wire; see driveRouteResponse.
	StartTime   string
	RoutePoints json.RawMessage
}

// DriveRouteFetcher fetches a single drive's route by drive id.
// Implementations MUST return an error wrapping sdk.ErrNotFound when the
// driveId is unknown (the handler maps that to 404). store.ErrDriveNotFound
// already wraps sdk.ErrNotFound, so the adapter only needs to pass it
// through with %w.
type DriveRouteFetcher interface {
	GetDriveRoute(ctx context.Context, driveID string) (DriveRouteData, error)
}

// driveRouteResponse is the wire shape for GET /api/drives/{driveId}/route
// (rest-api.md §7.4): the drive id plus the breadcrumb polyline. The
// route points are passed through as raw JSON — their per-point shape
// ({lat,lng,speed,heading,timestamp}) is owned by the writer that
// persisted them, not re-marshaled here.
type driveRouteResponse struct {
	DriveID     string          `json:"driveId"`
	RoutePoints json.RawMessage `json:"routePoints"`
}

// DriveRouteHandler handles GET /api/drives/{driveId}/route. It validates
// the caller's JWT, looks up the drive (404 if unknown), verifies the
// caller owns the drive's vehicle, projects the response through the
// role-based DriveRoute mask, and returns the polyline per rest-api.md
// §7.4 / §5.2.4. Mirrors VehicleDrivesHandler's auth/ownership/mask flow.
type DriveRouteHandler struct {
	// trips is the MYR-602 window gate. OPTIONAL and nil by default, which
	// leaves this endpoint owner-only exactly as MYR-369 left it — the
	// fail-closed direction for a deployment that has not wired trips.
	trips TripDriveAdmitter

	auth     tokenValidator
	vehicles VehicleSnapshotReader // owner check via GetByID(vehicleId)
	drives   DriveRouteFetcher
	roles    roleResolver // optional: nil disables role-based mask plumbing

	// Mask-audit fields (MYR-71, rest-api.md §5.3). All optional —
	// nil auditEmitter disables emit.
	auditEmitter  mask.AuditEmitter
	auditMetrics  mask.AuditMetrics
	auditEndpoint string

	logger *slog.Logger
}

// NewDriveRouteHandler creates a handler that serves
// GET /api/drives/{driveId}/route.
func NewDriveRouteHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	drives DriveRouteFetcher,
	logger *slog.Logger,
	opts ...DriveRouteOption,
) *DriveRouteHandler {
	h := &DriveRouteHandler{
		auth:     tokens,
		vehicles: vehicles,
		drives:   drives,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP handles GET /api/drives/{driveId}/route.
func (h *DriveRouteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	driveID := r.PathValue("driveId")
	if driveID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing driveId")
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}

	ctx := r.Context()

	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("drive route: invalid token", slog.String("error", err.Error()))
		status, code, message := wserrors.AuthFailure(err)
		h.writeError(w, status, code, message)
		return
	}

	// Fetch the drive first — we need its vehicleId to authorize. A
	// missing drive is a 404 before we reveal anything else.
	data, err := h.drives.GetDriveRoute(ctx, driveID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "drive not found")
			return
		}
		h.logger.Error("drive route: fetch failed",
			slog.String("drive_id", driveID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	if !h.verifyOwnership(ctx, w, driveID, data.VehicleID, data.StartTime, userID) {
		return
	}

	h.writeMaskedRoute(r, w, userID, data)
}

// verifyOwnership resolves the caller's access to the drive's vehicle: the
// OWNER, and nobody else (MYR-369 — no share of any shape opens the drives
// surfaces). Returns true on success; on failure writes an HTTP error and
// returns false.
// A drive that points at a missing vehicle is a data-integrity fault
// (500), distinct from an ownership mismatch (403, vehicle_not_owned).
// writeMaskedRoute resolves the caller's role, projects the route through
// the DriveRoute mask, and writes the response. When no roleResolver is
// configured the projection runs against auth.RoleOwner.
func (h *DriveRouteHandler) writeMaskedRoute(r *http.Request, w http.ResponseWriter, userID string, data DriveRouteData) {
	role := auth.RoleOwner
	if h.roles != nil {
		resolved, err := h.roles.ResolveRole(r.Context(), userID, data.VehicleID)
		if err != nil {
			h.logger.Error("drive route: role resolution failed",
				slog.String("vehicle_id", data.VehicleID),
				slog.String("error", err.Error()),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return
		}
		role = resolved
	}

	// Empty/absent breadcrumb → an empty array, never null.
	routePoints := data.RoutePoints
	if len(routePoints) == 0 {
		routePoints = json.RawMessage("[]")
	}

	maskSpec := mask.For(mask.ResourceDriveRoute, role)
	projected, fieldsMasked := mask.Apply(map[string]any{"routePoints": routePoints}, maskSpec)

	h.maybeEmitAudit(r, userID, data.VehicleID, role, fieldsMasked)

	// Honor the mask: if a (future) role strips routePoints, return [].
	out := json.RawMessage("[]")
	if v, ok := projected["routePoints"].(json.RawMessage); ok {
		out = v
	}

	h.writeJSON(w, http.StatusOK, driveRouteResponse{DriveID: data.DriveID, RoutePoints: out})
}

// maybeEmitAudit evaluates the REST audit-emit gate. Audit emits once per
// request per rest-api.md §5.3. For DriveRoute both roles see the single
// `routePoints` field, so this never fires today — it's plumbed for when
// sharing/viewer masking lands (FR-5.1), mirroring the drives-list handler.
func (h *DriveRouteHandler) maybeEmitAudit(r *http.Request, userID, vehicleID string, role auth.Role, fieldsMasked []string) {
	if h.auditEmitter == nil {
		return
	}
	if len(fieldsMasked) == 0 {
		return
	}
	requestID := requestIDFromRequest(r)
	if !mask.ShouldAuditREST(userID, requestID, vehicleID) {
		return
	}

	entry, err := mask.BuildEntry(
		userID,
		mask.TargetRESTResponse,
		vehicleID,
		role,
		mask.AuditChannelREST,
		fieldsMasked,
		h.auditEndpoint,
	)
	if err != nil {
		h.logger.Warn("drive route: BuildEntry failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	mask.EmitAsync(r.Context(), h.auditEmitter, h.auditMetrics, h.logger, entry)
}

// writeJSON marshals v as JSON with the given status code.
func (h *DriveRouteHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("drive route: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1).
func (h *DriveRouteHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
