package telemetry

// Recognising Tesla's OWNER-ONLY refusal of a fleet-telemetry config push
// (MYR-599), and keeping it out of the transient `push_failed` bucket where it
// would masquerade as something a retry could fix.
//
// ── THE UPSTREAM FACT ────────────────────────────────────────────────────────
//
// `POST /api/1/vehicles/fleet_telemetry_config` is OWNER-ONLY. A token whose
// `access_type` for the VIN is DRIVER gets:
//
//     HTTP 404  {"response": null, "error": "<VIN> not_found", "error_description": ""}
//
// while the SAME token can still list that VIN via `GET /api/1/vehicles` and can
// even DELETE its telemetry config. Visibility is not evidence of capability.
//
// Tesla does not document this. The `fleet_telemetry_config create` endpoint
// enumerates only `missing_key` / `unsupported_hardware` / `unsupported_firmware`
// / `max_configs` as rejection reasons, and says nothing about owner vs driver —
// even though the sibling `drivers` endpoint on the same page does carry the
// sentence "This endpoint is only available for the vehicle owner." The
// restriction is established by Tesla staff on GitHub:
//
//   - teslamotors/fleet-telemetry#116 — the reporter's "only Owners can POST
//     requests to fleet_telemetry_config" drew, from Tesla collaborator
//     aaronpkahn (2024-03-14): "Good point. That is inconsistent behavior. We'll
//     support DRIVER access on the POST endpoint soon".
//   - teslamotors/fleet-telemetry#126 — same collaborator, same day: "DRIVER
//     access will be supported soon. We'll publish the change to the
//     announcements section."
//
// As of 2026-09-05 the Fleet API announcements page carries no such entry, so
// the restriction is assumed to stand. If Tesla ships it silently this code
// still behaves correctly — the classification simply stops firing.
//
// ── WHY IT IS NOT SPELLED `driver_access_denied` ─────────────────────────────
//
// The 404 is GENERIC. The identical answer is what an account whose Tesla grant
// was revoked gets, and what a VIN genuinely absent from the account gets. We
// must not read it as "this caller is a driver" — we only know Tesla will not
// configure this car for this authorization. The label says exactly that much,
// and the honest user-facing advice ("reconnect your Tesla account") is right
// for the commonest causes and harmless for the rest.
//
// ── WHY IT IS NOT `push_failed` ──────────────────────────────────────────────
//
// `push_failed` is a TRANSIENT label by construction: the derivation lets it
// decay into `configuring` while a pairing epoch is live, and the reconciler
// retries it on a backoff forever. Neither is honest about a standing refusal. A
// card reading "connecting…" about a car Tesla will never configure is precisely
// the slow lie MYR-491's honesty bar was written against, and the retries are
// spent traffic against an answer that cannot change.

import (
	"errors"
	"net/http"
	"strings"
)

// isOwnerAccessRefusal reports whether err is Tesla's standing refusal to
// configure this VIN for this authorization.
//
// DELIBERATELY NARROW: a 404 whose body names `not_found`, and nothing else. A
// 403 is not included (Tesla does not use one here, and reading a future 403 as
// this would be a guess); a 404 from the PROXY rather than from Tesla carries a
// different body and does not match.
//
// The body check is what keeps this from swallowing an unrelated 404 — a
// mistyped proxy path, say — into a state that tells the owner to reconnect
// their Tesla account. If Tesla's phrasing changes, this stops matching and the
// outcome falls back to `push_failed`: degraded to the old behaviour, never
// wrong in a new way.
func isOwnerAccessRefusal(err error) bool {
	var apiErr *FleetAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != http.StatusNotFound {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Body), "not_found")
}
