package telemetry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// DriveDetailHandler handles GET /api/drives/{driveId}. It validates the
// caller's JWT, looks up the drive (404 if unknown), verifies the caller
// owns the drive's vehicle, projects the response through the role-based
// DriveDetail mask, and returns the full FR-3.4 record (minus
// routePoints) per rest-api.md §7.3 / §5.2.3. Mirrors DriveRouteHandler's
// auth/ownership/mask flow.
type DriveDetailHandler struct {
	// trips is the MYR-602 window gate. OPTIONAL and nil by default, which
	// leaves this endpoint owner-only exactly as MYR-369 left it — the
	// fail-closed direction for a deployment that has not wired trips.
	trips TripDriveAdmitter

	auth     tokenValidator
	vehicles VehicleSnapshotReader // owner check via GetByID(vehicleId)
	drives   DriveDetailFetcher
	roles    roleResolver // optional: nil disables role-based mask plumbing

	// Mask-audit fields (MYR-71, rest-api.md §5.3). All optional —
	// nil auditEmitter disables emit.
	auditEmitter  mask.AuditEmitter
	auditMetrics  mask.AuditMetrics
	auditEndpoint string

	logger *slog.Logger
}

// DriveDetailOption configures optional dependencies on
// DriveDetailHandler.
type DriveDetailOption func(*DriveDetailHandler)

// WithDriveDetailRoleResolver enables role-based field masking on the
// handler. Owners and viewers share the DriveDetail allow-list per
// rest-api.md §5.2.3, so this is plumbed for FR-5.1 sharing readiness —
// the mask is a no-op for both roles today.
// WithDriveDetailTripAdmitter opens §7.3 to a TRIP PARTICIPANT (MYR-602) for
// drives inside a window they were part of. See WithDrivesTripAdmitter for why
// a trip is a seam the drives surfaces may have and a share is not.
//
// Inert unless the composition root passes it; the handler stays owner-only
// without it.
func WithDriveDetailTripAdmitter(trips TripDriveAdmitter) DriveDetailOption {
	return func(h *DriveDetailHandler) {
		h.trips = trips
	}
}

func WithDriveDetailRoleResolver(roles roleResolver) DriveDetailOption {
	return func(h *DriveDetailHandler) {
		h.roles = roles
	}
}

// WithDriveDetailMaskAudit attaches a mask-audit emitter (MYR-71,
// rest-api.md §5.3). endpoint is the route pattern written to
// metadata.endpoint. emitter MAY be nil — in which case this is a no-op.
func WithDriveDetailMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics, endpoint string) DriveDetailOption {
	return func(h *DriveDetailHandler) {
		if emitter == nil {
			return
		}
		h.auditEmitter = emitter
		if metrics == nil {
			metrics = mask.NoopAuditMetrics{}
		}
		h.auditMetrics = metrics
		h.auditEndpoint = endpoint
	}
}

// NewDriveDetailHandler creates a handler that serves
// GET /api/drives/{driveId}.
func NewDriveDetailHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	drives DriveDetailFetcher,
	logger *slog.Logger,
	opts ...DriveDetailOption,
) *DriveDetailHandler {
	h := &DriveDetailHandler{
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

// ServeHTTP handles GET /api/drives/{driveId}.
func (h *DriveDetailHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		h.logger.Warn("drive detail: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	// Fetch the drive first — we need its vehicleId to authorize. A
	// missing drive is a 404 before we reveal anything else.
	data, err := h.drives.GetDriveDetail(ctx, driveID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "drive not found")
			return
		}
		h.logger.Error("drive detail: fetch failed",
			slog.String("drive_id", driveID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// ONE GATE FOR BOTH DRIVE READS (MYR-614): §7.3 and §7.4 resolve the
	// same access question over the same embedded facts, in one function.
	if !verifyDriveAccess(ctx, w, h.vehicles, h.trips, h.logger, "drive detail", data.DriveAccessFacts, userID) {
		return
	}

	h.writeMaskedDetail(r, w, userID, data)
}

// writeMaskedDetail resolves the caller's role, projects the detail
// through the DriveDetail mask, and writes the response. When no
// roleResolver is configured the projection runs against auth.RoleOwner.
func (h *DriveDetailHandler) writeMaskedDetail(r *http.Request, w http.ResponseWriter, userID string, data DriveDetailData) {
	role := auth.RoleOwner
	if h.roles != nil {
		resolved, err := h.roles.ResolveRole(r.Context(), userID, data.VehicleID)
		if err != nil {
			h.logger.Error("drive detail: role resolution failed",
				slog.String("vehicle_id", data.VehicleID),
				slog.String("error", err.Error()),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return
		}
		role = resolved
	}

	maskSpec := mask.For(mask.ResourceDriveDetail, role)
	projected, fieldsMasked := mask.Apply(buildDriveDetail(data).toMaskMap(), maskSpec)

	h.maybeEmitAudit(r, userID, data.VehicleID, role, fieldsMasked)

	h.writeJSON(w, http.StatusOK, projected)
}

// maybeEmitAudit evaluates the REST audit-emit gate. Audit emits once
// per request per rest-api.md §5.3. For DriveDetail both roles see the
// same field set, so this never fires today — it's plumbed for when
// sharing/viewer masking lands (FR-5.1), mirroring the route handler.
func (h *DriveDetailHandler) maybeEmitAudit(r *http.Request, userID, vehicleID string, role auth.Role, fieldsMasked []string) {
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
		h.logger.Warn("drive detail: BuildEntry failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	mask.EmitAsync(r.Context(), h.auditEmitter, h.auditMetrics, h.logger, entry)
}

// writeJSON marshals v as JSON with the given status code.
func (h *DriveDetailHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("drive detail: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1).
func (h *DriveDetailHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
