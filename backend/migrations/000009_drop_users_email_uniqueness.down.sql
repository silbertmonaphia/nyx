-- Pre-flight: refuse to restore the constraint if duplicate emails would block it.
-- Under normal operation no migration would have created the duplicates since the
-- column was already unique, but if someone ran this migration up, inserted two
-- rows with the same email, then ran it back, the ADD CONSTRAINT would fail with
-- an opaque message; this makes the failure actionable.
DO $$
BEGIN
    IF EXISTS (
        SELECT email FROM users
        WHERE email IS NOT NULL
        GROUP BY email HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot restore users_email_key: duplicate emails exist';
    END IF;
END$$;

ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
