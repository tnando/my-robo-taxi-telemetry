-- 0070_try_timestamptz.up.sql
--
-- MYR-608 review round: A TRY-CAST FOR `Drive."startTime"`, BECAUSE POSTGRES
-- HAS NO `TRY_CAST` AND THE REGEX THAT STOOD IN FOR ONE WAS NOT A GUARD.
--
-- ── WHAT THIS REPLACES, AND WHY IT HAD TO GO ────────────────────────────────
--
-- `Drive."startTime"` is a Prisma-owned TEXT column holding RFC 3339, and this
-- repo may not change its type. Every statement that needs the drive's start
-- INSTANT — the trip-window membership test, §7.30.7's bounds, the participant
-- narrowing, the drive totals — therefore casts it, and a bare `::timestamptz`
-- does not skip a row it cannot read: it ERRORS, and an error anywhere in a
-- statement fails the WHOLE statement. §7.2 is an owner's entire drive history,
-- so one unreadable value in a car's past is a permanent 500 on that list.
--
-- MYR-608 first guarded the cast with a prefix regex inside a `CASE`, which is
-- the one construct Postgres guarantees will not evaluate its `THEN` arm. The
-- review round found that guard admits values it cannot cast:
--
--     '2026-13-45T00:00:00Z'   -- month 13, day 45  → matched, then ERRORED
--     '2026-02-30T08:00:00Z'   -- February 30th     → matched, then ERRORED
--     '2026-01-01T25:00:00Z'   -- hour 25           → matched, then ERRORED
--
-- The regex counted DIGITS. It could not count MONTHS, and no regex can decide
-- whether a date exists — that is a calendar question, and the only thing in
-- the database that can answer it is the cast itself. A guard that has to
-- re-implement the parser it is guarding is the wrong shape of guard.
--
-- ── SO THE CAST GUARDS ITSELF ───────────────────────────────────────────────
--
-- The only construct that can catch a cast failure in Postgres is a PL/pgSQL
-- `EXCEPTION` block. This function is the smallest one that does it, and it is
-- the ONE place in the platform where a text→instant conversion may fail
-- softly: an unreadable row resolves NULL, belongs to no trip window, satisfies
-- neither bound of any range, and is counted in no total. It is never told to a
-- reader as an error — the same direction MYR-614 settled on for the
-- single-drive gate: a data fault is an operator's problem, not a reader's.
--
-- ── GO-OWNED, AND CG-DL-9 PERMITS IT ────────────────────────────────────────
--
-- CG-DL-9 forbids a Go migration from NAMING a Prisma-owned table (`Drive`,
-- `Vehicle`, `User`, …). This file names none: it creates a `go_`-prefixed
-- FUNCTION over `text`, which is a Go-owned database object in exactly the
-- sense every `go_*` table is. Prisma neither knows nor needs to know about it,
-- and `prisma db pull` filters it out on the prefix like everything else.
--
-- ── THE VOLATILITY LABEL IS DELIBERATE, AND IT IS A SIMPLIFICATION ──────────
--
-- `text::timestamptz` is strictly STABLE, not IMMUTABLE. Two classes of input
-- make it so, and both were confirmed against Postgres 16 rather than assumed:
-- a string carrying no offset (`'2026-01-01 10:00:00'`) is resolved against the
-- session's `TimeZone` GUC, and Postgres also accepts the SPECIAL VALUES
-- `'now'`, `'today'`, `'yesterday'`, `'tomorrow'` and `'epoch'`, of which the
-- first four read the clock (`'now'` → the transaction's start instant).
--
-- It is declared IMMUTABLE here anyway, for two reasons that hold together and
-- would not hold apart:
--
--   1. Every caller passes a COLUMN REFERENCE, never a literal, so there is
--      nothing for the planner to constant-fold at plan time — which is the one
--      way an over-declared volatility produces a wrong answer.
--   2. Every writer in both repos writes RFC 3339 WITH an offset, so neither
--      the GUC nor the special values enter into the data that exists.
--
-- IMMUTABLE (rather than STABLE) is what lets the planner treat the expression
-- as a constant per row and, if this ever needs one, lets an expression index
-- be built over it. If a caller ever passes a LITERAL, or a writer ever emits
-- an offsetless instant or one of the special values, this label becomes a lie
-- and must be downgraded to STABLE — recorded here so the next reader does not
-- have to re-derive it.
--
-- ⚠ A BARE DATE IS READABLE, and that is the cast's answer, not a concession.
-- `'2026-07-02'::timestamptz` is `2026-07-02 00:00:00+00`. The regex this
-- function replaces demanded a time component and so blanked such a row on
-- §7.2 while the strict `WHERE` casts on the other surfaces placed the SAME
-- drive in the SAME window — the two surfaces disagreeing about one drive, in
-- a change whose subject is making them agree. Every surface now gives the
-- cast's answer. internal/store/trip_drive_totals_test.go pins it.
--
-- ⚠ IT IS PARALLEL UNSAFE (the default), DELIBERATELY, AND MARKING IT SAFE IS A
-- RUNTIME ERROR RATHER THAN A MISSED OPTIMISATION. The first draft of this file
-- declared PARALLEL SAFE, reasoning that the function is used in WHERE clauses
-- scanning a whole vehicle's drive history and that disabling parallel plans
-- there would be a real cost. Measuring it produced:
--
--     ERROR: cannot start subtransactions during a parallel operation
--
-- An `EXCEPTION` block IS a subtransaction, and a parallel worker may not open
-- one. PostgreSQL's own parallel-safety documentation names this case: a
-- PL/pgSQL function that establishes an EXCEPTION block to catch errors changes
-- the transaction state and MUST be marked PARALLEL UNSAFE. A SAFE label here
-- would have turned a large car's drive list into a 500 the moment the planner
-- chose a parallel plan — the exact failure mode this function exists to
-- prevent, reintroduced by the label on the function preventing it.
--
-- PARALLEL RESTRICTED is not the escape either: it permits execution in the
-- leader during parallel mode, which is still parallel mode.
--
-- THE COST IS REAL AND IS PAID SOMEWHERE ELSE. Statements calling this function
-- cannot use a parallel plan, so §7.30.7, the participant narrowing and the
-- totals scan a vehicle's history serially. The measured answer is the index in
-- the PR body's finding-10 note: an ordered `("vehicleId","startTime" DESC,
-- "id" DESC)` index on the Prisma-owned `Drive` table lets §7.2 walk the LIMIT
-- in order and never sort at all, which is worth far more than the parallelism
-- (0.59 ms vs 21.6 ms on a 60k-drive vehicle). It cannot be added from here.
--
-- STRICT short-circuits the NULL input without entering the exception block at
-- all, which is the common case for a `Drive` row written but not yet started.
--
-- `search_path` is pinned so the body cannot be captured by a schema planted
-- ahead of `pg_catalog`; the type name is fully qualified for the same reason.
--
-- ── CLASSIFICATION ──────────────────────────────────────────────────────────
--
-- P0. A pure function over a text value. It reads no table, writes nothing,
-- logs nothing, and holds no state.

CREATE OR REPLACE FUNCTION go_try_timestamptz(value text)
RETURNS timestamptz
LANGUAGE plpgsql
IMMUTABLE
STRICT
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    RETURN value::pg_catalog.timestamptz;
EXCEPTION
    WHEN others THEN
        RETURN NULL;
END;
$$;

COMMENT ON FUNCTION go_try_timestamptz(text) IS
    'MYR-608: try-cast for the Prisma-owned Drive."startTime" TEXT column. Returns NULL where a bare ::timestamptz would fail the whole statement.';
