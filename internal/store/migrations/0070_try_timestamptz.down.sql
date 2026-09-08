-- 0070_try_timestamptz.down.sql
--
-- Drops the try-cast.
--
-- IT BREAKS EVERY STATEMENT THAT CALLS IT AND SAYS SO. `driveStartInstantExpr`
-- in internal/store/trip_queries.go is a `const` naming this function, so after
-- this runs the §7.2 drive list, §7.30.7, the participant narrowing and the
-- trip totals all fail with `function go_try_timestamptz(text) does not exist`
-- until a binary predating MYR-608 is deployed alongside.
--
-- That is the honest shape of the rollback: the function is not decoration, it
-- is the guard those statements are built on. The down file exists because
-- every migration in this directory has one.

DROP FUNCTION IF EXISTS go_try_timestamptz(text);
