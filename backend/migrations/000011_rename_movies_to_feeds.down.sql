-- Reverse the rename. Indexes/constraints/sequence still point at
-- whatever table name is current, so the inverse rename restores the
-- pre-migration state.
ALTER TABLE feeds RENAME TO movies;