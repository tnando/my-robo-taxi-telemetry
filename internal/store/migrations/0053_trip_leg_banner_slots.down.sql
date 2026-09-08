-- Reverses 0053. Dropping the table restores the pre-MYR-620 behaviour: every
-- leg that opens sends its own banner, and a flapping detector sends one each.
DROP TABLE IF EXISTS go_trip_leg_banners;
