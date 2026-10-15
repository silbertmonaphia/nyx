-- Reverse 000012_add_user_id_to_feeds. Drop in reverse dependency
-- order: FK → index → column.
ALTER TABLE feeds DROP CONSTRAINT feeds_user_id_fkey;

DROP INDEX feeds_user_id_idx;

ALTER TABLE feeds DROP COLUMN user_id;
