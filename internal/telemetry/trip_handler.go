package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// The MYR-602 TRIPS surface (rest-api.md §7.30). One handler, nine routes:
//
//	POST   /api/vehicles/{vehicleId}/trips
//	GET    /api/trips
//	GET    /api/trips/{tripId}
//	PATCH  /api/trips/{tripId}
//	POST   /api/trips/{tripId}/end
//	DELETE /api/trips/{tripId}/participants/me
//	GET    /api/trips/{tripId}/drives
//	POST   /api/trips/{tripId}/activity-start-token
//	DELETE /api/trips/{tripId}/activity-start-token
//
// ONE HANDLER TYPE rather than one per route, because all nine share the same
// three things: the token validator, the store, and the 404-not-403 rule. Nine
// constructors would be nine chances to wire one of them without the rule.
//
// THE 404-NOT-403 RULE, stated once here and enforced by the store: every
// per-trip route answers 404 to a caller who is not on the trip — identically
// to how it answers for a trip that does not exist. A 403 would confirm the id
// is real, and trip ids are the kind of thing that ends up in a deep link.
// `vehicle_not_owned` (403) appears exactly once on this surface, on CREATE,
// where the caller supplied a VEHICLE id whose existence they have already
// established through the catalog.

// tripBodyLimit caps a decoded request body. Trips carry a 60-character name
// and a list of share ids; 16 KiB is generous by two orders of magnitude and
// exists so a malformed or hostile body is refused before it is parsed.
const tripBodyLimit = 16 << 10

// TripHandler serves §7.30.
type TripHandler struct {
	auth     tokenValidator
	trips    TripStore
	vehicles VehicleSnapshotReader

	// notifier delivers the three REST-caused `trips` pushes. NEVER nil after
	// construction — the constructor substitutes a no-op — so the call sites
	// carry no nil checks and a fourth event added later cannot forget one.
	notifier TripNotifier

	// enabled is the TRIPS_ENABLED kill switch. FALSE MAKES EVERY ROUTE 503,
	// including the reads: a feature that can be switched off has to be
	// switched off whole, and leaving GET alive would show an owner a live
	// trip card whose every button returns an error.
	enabled bool

	logger *slog.Logger
}

// TripOption configures optional dependencies.
type TripOption func(*TripHandler)

// WithTripNotifier attaches the push fan-out (MYR-602 §7.19 category `trips`).
//
// OMITTING IT IS SAFE AND SILENT: trips are created, ended and joined exactly
// as they would be, and nobody is told. A push is an announcement about a state
// change, never the state change itself, so a missing notifier must not be able
// to fail a create.
func WithTripNotifier(n TripNotifier) TripOption {
	return func(h *TripHandler) {
		if n != nil {
			h.notifier = n
		}
	}
}

// NewTripHandler builds the §7.30 handler. `enabled` is the TRIPS_ENABLED kill
// switch, passed rather than read here so the whole configuration surface stays
// in internal/config and a test can set it directly.
func NewTripHandler(
	tokens tokenValidator,
	trips TripStore,
	vehicles VehicleSnapshotReader,
	enabled bool,
	logger *slog.Logger,
	opts ...TripOption,
) *TripHandler {
	h := &TripHandler{
		auth:     tokens,
		trips:    trips,
		vehicles: vehicles,
		notifier: noopTripNotifier{},
		enabled:  enabled,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// begin runs the preamble every route shares: the kill switch, the bearer
// token, and the trip id where the route has one.
//
// Returns ok=false having already written the response. The KILL SWITCH IS
// CHECKED FIRST, before authentication, deliberately: when the feature is off
// the answer is the same for everybody and there is no reason to validate a
// token to say so.
func (h *TripHandler) begin(w http.ResponseWriter, r *http.Request) (context.Context, string, bool) {
	if !h.enabled {
		// 503, not 404. The routes EXIST and will work again; a 404 would tell
		// a client to stop asking, and some clients cache that decision.
		h.writeError(w, http.StatusServiceUnavailable, wserrors.ErrCodeServiceUnavailable,
			"trips are temporarily unavailable")
		return nil, "", false
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return nil, "", false
	}
	ctx := r.Context()
	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("trips: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return nil, "", false
	}
	return ctx, userID, true
}

// ServeList handles GET /api/trips?status=&limit=.
func (h *TripHandler) ServeList(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := h.begin(w, r)
	if !ok {
		return
	}

	status := r.URL.Query().Get("status")
	switch status {
	case "", tripStatusScheduled, tripStatusActive, tripStatusEnded:
	default:
		// Refused rather than ignored. A client that asked for `?status=activE`
		// and received every trip would render the wrong list and never learn
		// why; silently widening a filter is the failure mode a typed enum
		// exists to prevent.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"status must be scheduled, active or ended")
		return
	}

	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > tripListMaxLimit {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
				"limit must be an integer in [1, 100]")
			return
		}
		limit = n
	}

	trips, err := h.trips.ListTrips(ctx, userID, status, limit)
	if err != nil {
		h.failTrip(w, "list", "", err)
		return
	}

	// The envelope is `{items: [...]}` with NO cursor, and that is a contract
	// statement rather than an omission: a person has a handful of trips, not a
	// feed, and an SDK pagination helper must not mistake this for a page and
	// go looking for a cursor that will never be there.
	items := make([]map[string]any, 0, len(trips))
	for i := range trips {
		items = append(items, tripWire(trips[i], userID))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// verifyVehicleOwner is the CREATE gate. Owner and nobody else — a share, of
// any tier, never lets somebody open a window on a car they do not own.
//
// The 404 arm is for an unknown vehicle and the 403 arm for one the caller does
// not own, matching every other per-vehicle surface.
func (h *TripHandler) verifyVehicleOwner(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return false
		}
		h.logger.Error("trips: vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}
	if _, err := vehicleAccessForOwnerOnly(ctx, userID, row.UserID); err != nil {
		denyVehicleAccess(w, h.logger, "trips", vehicleID, userID)
		return false
	}
	return true
}

// decode reads a size-capped, STRICT JSON body. Unknown fields are refused, so
// a client sending a field this server version does not know finds out rather
// than having it silently dropped.
func (h *TripHandler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, tripBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return false
	}
	return true
}

// failTrip maps a store error onto the wire, logging the ones that are ours
// rather than the caller's.
//
// The identifier in the log is a cuid (P0). The trip NAME never appears —
// it is P1 user content, and an error path is the one place a value reliably
// reaches a log without anybody deciding it should.
func (h *TripHandler) failTrip(w http.ResponseWriter, op, subject string, err error) {
	if h.writeTripError(w, err) {
		return
	}
	h.logger.Error("trips: "+op+" failed",
		slog.String("subject", subject),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
}

func (h *TripHandler) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.Error("trips: encode response failed", slog.String("error", err.Error()))
	}
}

func (h *TripHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}

func (h *TripHandler) writeErrorSub(w http.ResponseWriter, status int, code wserrors.ErrorCode, sub wserrors.SubCode, msg string) {
	wserrors.WriteErrorEnvelopeSub(w, h.logger, status, code, sub, msg)
}
