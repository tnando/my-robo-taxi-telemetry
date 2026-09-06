// POST /api/tesla/vehicles/{vehicleId}/acknowledge-owner-approval — the consent
// gate in front of configuring a car its linker DRIVES but does not OWN
// (rest-api.md §7.29, MYR-599, contracts v0.39.0).
//
// ── A NOTE ON THE SECTION NUMBER, BECAUSE IT DOES NOT MATCH THE CONTRACT ─────
//
// Contracts v0.39.0 — and therefore the three vendored schemas under
// docs/contracts/schemas/ — call this endpoint §7.24. It is §7.29 here. §7.24
// in this repo's rest-api.md has been `POST /api/ride-requests/join` since
// 2026-08-13 (MYR-540), and §7.25 through §7.28 are likewise occupied, so the
// contract's number collides with a stable, shipped section. Renumbering five
// live sections to satisfy a citation would break every cross-reference that
// points at them, in this file and in others, so the endpoint took the next
// free number and the collision is recorded as a divergence in rest-api.md §10
// rather than papered over. A schema saying §7.24 about the owner-approval
// acknowledgment means THIS section.
//
// ── WHAT THIS ENDPOINT ACTUALLY RECORDS ──────────────────────────────────────
//
// Not a permission. EVIDENCE. The platform cannot verify with Tesla that an
// owner approved anything — no Fleet API exposes such a fact — so what it holds
// instead is an attributable, append-only record that a named account, at a
// named instant, was shown a named version of a named text and said yes. That
// is the artifact that would be produced if an owner ever objected, and
// pretending to more than that would be the dishonesty this whole feature was
// designed around.
//
// It follows that the server stores the VERSION ID and nothing else. Not the
// rendered copy (a published document with a stable id, which must not vary per
// row), not the VIN (P1), not the owner (whom this platform cannot name).
//
// ── WHY IT ANSWERS IN §7.23's SHAPE ──────────────────────────────────────────
//
// VehicleSetupCompletionResponse, deliberately, so a client reuses its existing
// completion handling verbatim: adopt the returned setupState and let the
// existing cards take over — usually `awaiting_virtual_key` next, at which point
// the pair-key card is already written.
//
// ── THE THREE 200s ───────────────────────────────────────────────────────────
//
// All of them are 200, and each is 200 for its own reason:
//
//  1. FIRST ACKNOWLEDGMENT. The row is stamped, the audit row written, the push
//     attempted.
//  2. REPEAT on an already-acknowledged car. NEVER a 409. It re-runs the push
//     and answers with the current state, which makes it a safe retry for a
//     client that lost the first response and a usable "try again" for a push
//     that failed transiently. The STORED acknowledgment is not overwritten:
//     the FIRST one is what the platform would point to, so a later call cannot
//     move the instant it happened.
//  3. OWNER-ACCESS CAR. Nothing to acknowledge, so nothing is recorded and NO
//     AUDIT ROW IS WRITTEN — an audit trail that recorded non-events would be
//     worse than useless in the one conversation it exists for. A client that
//     cannot tell the two cases apart is never punished for asking.
//
// ── 404 AND NOT 403 ON AN OWNERSHIP MISMATCH ─────────────────────────────────
//
// This is a DELIBERATE DIVERGENCE from its siblings §7.12 / §7.15 / §7.23, all
// of which answer 403 `vehicle_not_owned` on a real mismatch, and it is what
// contracts v0.39.0 specifies for this endpoint. An endpoint whose entire job is
// recording a consent must not double as a way to enumerate which vehicleIds
// exist, so the unknown case and the not-yours case are made indistinguishable.
// (The v0.39.0 schema text asserts §7.23 does the same; it does not — §7.23
// still answers 403. That divergence is real and recorded, not copied.)

package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// acknowledgmentVersionMaxRunes bounds the stored version id.
//
// RUNES, NOT BYTES, matching the MYR-581 name cap: a caller sending a
// multi-byte string should be refused on what it MEANS, not on how it happens
// to encode. The id is a lowercase slug in practice, so this is generous by
// design — the cap exists to stop an unbounded write, not to police a format.
const acknowledgmentVersionMaxRunes = 64

// OwnerApprovalRecorder stamps the acknowledgment and writes its audit row in
// one transaction. Reports whether there was a driver-access row to stamp.
// Satisfied by *store.VehicleRepo.
//
// Consumer-site interface, and the bool is the load-bearing return: it is how
// the handler tells "acknowledged" from "there was nothing to acknowledge", the
// two 200s that must not be collapsed.
type OwnerApprovalRecorder interface {
	AcknowledgeOwnerApproval(ctx context.Context, vehicleID, userID, version string, now time.Time) (recorded bool, err error)
}

// OwnerApprovalHandler serves the §7.29 route.
type OwnerApprovalHandler struct {
	auth      tokenValidator
	vehicles  VehicleSnapshotReader
	recorder  OwnerApprovalRecorder
	completer setupCompleter
	logger    *slog.Logger
	now       func() time.Time
}

// NewOwnerApprovalHandler wires the handler. completer is the SAME
// *SetupCompleter §7.23 uses, which is what makes "the same best-effort push
// complete-setup performs" a structural fact rather than a promise.
func NewOwnerApprovalHandler(
	auth tokenValidator,
	vehicles VehicleSnapshotReader,
	recorder OwnerApprovalRecorder,
	completer setupCompleter,
	logger *slog.Logger,
) *OwnerApprovalHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &OwnerApprovalHandler{
		auth:      auth,
		vehicles:  vehicles,
		recorder:  recorder,
		completer: completer,
		logger:    logger,
		now:       time.Now,
	}
}

// acknowledgeOwnerApprovalRequest is the §7.29 body.
//
// STRICT DECODE (DisallowUnknownFields below): a client sending a field this
// server does not know is sending something it believes matters, and silently
// dropping it on a CONSENT record is the wrong failure. The same discipline
// MYR-581's PATCH /api/users/me applies.
type acknowledgeOwnerApprovalRequest struct {
	AcknowledgmentVersion string `json:"acknowledgmentVersion"`
}

// ServeHTTP routes POST only.
func (h *OwnerApprovalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}
	h.handle(w, r)
}

func (h *OwnerApprovalHandler) handle(w http.ResponseWriter, r *http.Request) {
	vehicleID := strings.TrimSpace(r.PathValue("vehicleId"))
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
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
		h.logger.Warn("acknowledge-owner-approval: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	version, ok := h.decodeVersion(w, r)
	if !ok {
		return
	}

	row, ok := h.authorizeVehicle(ctx, w, vehicleID, userID)
	if !ok {
		return
	}

	recorded, err := h.recorder.AcknowledgeOwnerApproval(ctx, row.ID, userID, version, h.now().UTC())
	if err != nil {
		h.logger.Error("acknowledge-owner-approval: could not record the acknowledgment",
			slog.String("vehicle_id", row.ID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}
	if recorded {
		// The headline event, and it is the one line an owner-side complaint
		// would be answered from — so it names the person, the car and the copy
		// version, and no VIN (P1).
		h.logger.Info("owner_approval_acknowledged",
			slog.String("event", "owner_approval_acknowledged"),
			slog.String("vehicle_id", row.ID),
			slog.String("user_id", userID),
			slog.String("acknowledgment_version", version))
	}

	h.completeSetup(ctx, w, row, userID, recorded)
}

// completeSetup runs the SAME best-effort push §7.23 runs and answers in its
// shape.
//
// THE ROW IS RE-READ FIRST, and that is not defensive tidiness: the row in hand
// was fetched BEFORE the stamp, so its DriverAccess still says
// "unacknowledged". Handing that stale row to the completer would walk straight
// into the gate this request just opened and return
// `awaiting_owner_acknowledgment` to the very call that satisfied it — the
// endpoint refusing its own effect.
//
// A FAILED PUSH IS NOT A FAILED ACKNOWLEDGMENT. The consent was genuinely
// given and is committed; only the Tesla-side step is uncertain. So an error
// from the completer is reported in §7.23's own vocabulary and the stamp
// stands — a client retries the whole call, which is idempotent by design.
func (h *OwnerApprovalHandler) completeSetup(
	ctx context.Context, w http.ResponseWriter, row VehicleSnapshotRow, userID string, recorded bool,
) {
	// `recorded` alone is NOT the right condition, and the extra disjunct closes
	// a real interleaving. Two genuinely concurrent acknowledgments of one car:
	// the store's `AND acknowledged_at IS NULL` makes the loser match zero rows,
	// so it gets recorded=false — and if that skipped the re-read it would hand
	// the completer the row it authorized against, which still says
	// unacknowledged, and answer `awaiting_owner_acknowledgment` to a request
	// contemporaneous with the one that satisfied it. The client would re-show
	// the sheet it had just confirmed.
	//
	// So: re-read whenever the row we hold says the gate is shut, whether this
	// call is what shut it or not.
	if recorded || row.DriverAccess.PendingAcknowledgment() {
		fresh, err := h.vehicles.GetByID(ctx, row.ID)
		if err != nil {
			// The acknowledgment is committed; we simply cannot see its effect.
			// Reporting a 500 would invite a retry that is safe but pointless,
			// and reporting the stale state would be a lie about the gate. The
			// truthful minimum is the state we can still derive honestly.
			h.logger.Error("acknowledge-owner-approval: recorded, but the vehicle could not be re-read",
				slog.String("vehicle_id", row.ID),
				slog.String("error", err.Error()))
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return
		}
		row = fresh
	}

	state, err := h.completer.Complete(ctx, row)
	if err != nil {
		h.logger.Warn("acknowledge-owner-approval: recorded, but the config push did not complete",
			slog.String("vehicle_id", row.ID),
			slog.String("user_id", userID),
			slog.String("error", redactedErrorText(err)))
		writeSetupCompletionError(w, h.logger, row, err)
		return
	}

	h.writeJSON(w, http.StatusOK, setupCompletionResponse{VehicleID: row.ID, SetupState: state})
}

// decodeVersion reads and validates the body.
//
// THE VERSION IS NOT CHECKED AGAINST A LIST OF KNOWN IDS, deliberately. An id
// this server does not recognise is still RECORDED AS SENT: a client shipped
// before a copy revision must not be blocked from finishing setup, and the
// stored string's job is to say which words a person actually saw — which is a
// question about the CLIENT's build, not about this server's. Refusing an
// unknown id would also make every copy change a coordinated deploy.
func (h *OwnerApprovalHandler) decodeVersion(w http.ResponseWriter, r *http.Request) (string, bool) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()

	var req acknowledgeOwnerApprovalRequest
	if err := dec.Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"body must be {\"acknowledgmentVersion\": \"<id>\"}")
		return "", false
	}

	version := strings.TrimSpace(req.AcknowledgmentVersion)
	if version == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"acknowledgmentVersion is required")
		return "", false
	}
	if utf8.RuneCountInString(version) > acknowledgmentVersionMaxRunes {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"acknowledgmentVersion is too long")
		return "", false
	}
	return version, true
}

// authorizeVehicle loads the vehicle and enforces caller ownership.
//
// BOTH FAILURES ANSWER 404 — see the file header for why this endpoint diverges
// from its 403-answering siblings.
func (h *OwnerApprovalHandler) authorizeVehicle(
	ctx context.Context, w http.ResponseWriter, vehicleID, userID string,
) (VehicleSnapshotRow, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return VehicleSnapshotRow{}, false
		}
		h.logger.Error("acknowledge-owner-approval: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return VehicleSnapshotRow{}, false
	}
	if row.UserID != userID {
		h.logger.Warn("acknowledge-owner-approval: ownership mismatch (answered 404)",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID))
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
		return VehicleSnapshotRow{}, false
	}
	return row, true
}

func (h *OwnerApprovalHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *OwnerApprovalHandler) writeError(
	w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string,
) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
