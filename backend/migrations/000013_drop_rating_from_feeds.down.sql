-- Reverse 000013_drop_rating_from_feeds. Re-add the rating column
-- with the same shape it carried originally (nullable DOUBLE
-- PRECISION) so a down migration stays consistent with the historical
-- schema. Previously-stored ratings are NOT recovered — the data was
-- discarded by the up migration.
ALTER TABLE feeds ADD COLUMN rating DOUBLE PRECISION;