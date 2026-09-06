package telemetry

import "github.com/myrobotaxi/telemetry/internal/mask"

// Optional-dependency constructors for the GET /api/vehicles/{vehicleId}/drives handler.
//
// NOTE (MYR-369): there is deliberately NO share-reader option here any more.
// The drives surfaces are owner-only again, so the handler has no seam a share
// could be passed through — re-opening them has to be a real change, not one
// wiring line.
// Split out of the handler file so it stays inside the 300-line cap; each
// option is inert unless the composition root passes it, and the handler
// fails CLOSED without it.

// VehicleDrivesOption configures optional dependencies on
// VehicleDrivesHandler.
type VehicleDrivesOption func(*VehicleDrivesHandler)

// WithDrivesRoleResolver enables role-based field masking on the
// handler. Owners and viewers share the DriveSummary allow-list per
// rest-api.md §5.2.2, so in v1 this is plumbed for FR-5.5 readiness.
func WithDrivesRoleResolver(roles roleResolver) VehicleDrivesOption {
	return func(h *VehicleDrivesHandler) {
		h.roles = roles
	}
}

// WithDrivesMaskAudit attaches a mask-audit emitter to the handler
// (MYR-71, rest-api.md §5.3). The endpoint argument is the route
// pattern written to metadata.endpoint —
// "/api/vehicles/{vehicleId}/drives" — rather than the substituted
// URL. emitter MAY be nil — in which case this option is a no-op.
func WithDrivesMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics, endpoint string) VehicleDrivesOption {
	return func(h *VehicleDrivesHandler) {
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

// WithDrivesTripAdmitter opens §7.2 to a TRIP PARTICIPANT (MYR-602), limited
// to the drives inside a window they were part of.
//
// It is the seam MYR-369's note above said should not exist for SHARES, and
// the distinction is the point: a share is a standing grant with no bound, and
// re-opening the surface to one would hand a viewer the car's whole history
// forever. A trip is a bounded set of instants the owner named, and what this
// option admits is exactly that set — enforced in the statement that applies
// the LIMIT, not by a filter over an owner's page.
//
// Inert unless the composition root passes it; the handler stays owner-only
// without it.
func WithDrivesTripAdmitter(trips TripDriveAdmitter) VehicleDrivesOption {
	return func(h *VehicleDrivesHandler) {
		h.trips = trips
	}
}
