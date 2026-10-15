-- Restores the column as nullable (matching the post-000009 state).
-- Going further back to UNIQUE NOT NULL would conflict with existing
-- rows that are NULL, and 000009 already governs the uniqueness story.
ALTER TABLE users ADD COLUMN email TEXT;