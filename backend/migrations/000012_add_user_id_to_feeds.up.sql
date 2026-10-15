-- Owner-scoping for feeds. Each feed belongs to exactly one user; only
-- the owner can read (via GET /api/feeds, which is now auth-required),
-- modify (PUT), or soft-delete (DELETE) it. Cross-owner writes return
-- 404 — single ErrNotFound sentinel, no leak.
--
-- Migration order matters:
--   1. Add nullable column so the ALTER doesn't fail on existing rows.
--   2. Self-heal the empty users case: if the DB has seed feed rows
--      (000002) but no registered user, insert a placeholder user so
--      the backfill has a row to assign. The placeholder owns every
--      pre-existing feed in this scenario; the production deploy path
--      is unaffected because production lands before any user-created
--      feed, so this only fires for a freshly-bootstrapped DB.
--   3. Backfill pre-existing rows.
--   4. Set NOT NULL + add FK + index.
--
-- The "first user wins" rule is intentional and documented in the PR
-- description. If a future hard-delete path is added, it must
-- reassign or delete the user's feeds first (no cascade).
ALTER TABLE feeds ADD COLUMN user_id BIGINT;

-- Bootstrap path: on a fresh DB, 000002 seeds 3 feed rows but no
-- user has registered yet. Without a user to assign feeds to, the
-- SET NOT NULL below would fail. Insert a sentinel placeholder so
-- the backfill has something to reference; production never hits
-- this branch because real users register before any feed is
-- created.
INSERT INTO users (id, username, password_hash)
VALUES (1, '__pre_owner_scope_seed__', 'unused')
ON CONFLICT (id) DO NOTHING;

UPDATE feeds SET user_id = (SELECT id FROM users WHERE deleted_at IS NULL ORDER BY id LIMIT 1)
WHERE user_id IS NULL;

ALTER TABLE feeds ALTER COLUMN user_id SET NOT NULL;

ALTER TABLE feeds ADD CONSTRAINT feeds_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id);

CREATE INDEX feeds_user_id_idx ON feeds(user_id);
