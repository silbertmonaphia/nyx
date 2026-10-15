-- Drop the rating column from feeds. The product no longer collects a
-- user-assigned rating, so the column is dead weight — removing it
-- shrinks the row, simplifies the API contract, and lets the
-- description-only schema breathe. Existing rows keep their other
-- fields unchanged.
ALTER TABLE feeds DROP COLUMN rating;