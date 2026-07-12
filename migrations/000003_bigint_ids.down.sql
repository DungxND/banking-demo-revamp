-- Downgrade BIGINT back to INTEGER.
-- ⚠️  This will FAIL if any row has an id > 2,147,483,647.

ALTER TABLE notifications
    ALTER COLUMN user_id TYPE INTEGER,
    ALTER COLUMN id      TYPE INTEGER;

ALTER TABLE transfers
    ALTER COLUMN amount    TYPE INTEGER,
    ALTER COLUMN to_user   TYPE INTEGER,
    ALTER COLUMN from_user TYPE INTEGER,
    ALTER COLUMN id        TYPE INTEGER;

ALTER TABLE users
    ALTER COLUMN balance TYPE INTEGER,
    ALTER COLUMN id      TYPE INTEGER;
