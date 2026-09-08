package telemetry

// DriveAccessFacts is the identity a single-drive read must carry in order to
// be ADMITTED — the three values every drive gate consumes, and nothing else.
//
// WHY THIS IS ONE SHARED, EMBEDDED SHAPE RATHER THAN THREE FIELDS COPIED INTO
// EACH READ MODEL (MYR-614).
//
// §7.3 (drive detail) and §7.4 (drive route) run the same gate: owner, or a
// trip participant whose window covers the instant THIS drive began. Both read
// models used to spell those fields out themselves, and the route model spelled
// out only two of the three — its adapter built `DriveRouteData{DriveID,
// VehicleID, RoutePoints}` and never set `StartTime`. The field defaulted to
// "", the RFC 3339 parse failed, and EVERY participant was refused 404 on EVERY
// route, including drives squarely inside their window. The handler tests set
// the field by hand, so nothing caught it; the owner path returns before the
// window check, so nobody who could have noticed was affected.
//
// Embedding is the repair that outlives the fix. The three fields now exist in
// exactly ONE type, produced at the composition root by exactly ONE function
// over a store drive row (`driveAccessFacts` in cmd/telemetry-server), and
// shared by both read models. A future field the gate needs is added here once
// and reaches both surfaces; a field dropped here breaks both surfaces' tests
// at once rather than silently disabling access on the quieter one. Callers are
// unaffected by the shape change — Go promotes the embedded fields, so
// `data.StartTime` still reads the same.
//
// NONE OF THESE ARE WIRE FIELDS BY VIRTUE OF BEING HERE. §7.4's response has
// never carried a start time and still does not; §7.3 emits its own `startTime`
// because the DriveDetail schema says so, not because the gate reads one.
type DriveAccessFacts struct {
	// DriveID is the drive's cuid — the id in the request path, echoed by
	// §7.4's response and by §7.3's `id`.
	DriveID string

	// VehicleID is the drive's vehicle. The gate resolves ownership against
	// it, and the mask layer resolves the caller's role against it.
	VehicleID string

	// StartTime is the drive's RFC 3339 start instant, as stored (Prisma
	// keeps it as a string column).
	//
	// REQUIRED ON EVERY PATH THAT BUILDS THESE FACTS. A trip participant is
	// admitted only to drives that began inside one of their windows, so an
	// absent or unparseable value is not a refusal — it is a data fault, and
	// since MYR-614 the handlers answer it 500 rather than let it masquerade
	// as a legitimate 404.
	StartTime string
}
