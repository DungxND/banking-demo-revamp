-- Migration 000003: Upgrade SERIAL (INTEGER) PKs/FKs to BIGINT
--
-- Postgres skill: "Use BIGINT for all IDs and foreign keys, even on small tables."
--
-- SERIAL uses INTEGER (4-byte, max ~2.1 billion). For a banking system processing
-- transfers, a busy system can hit this limit. BIGINT (8-byte) supports ~9.2 × 10^18.
-- BIGINT GENERATED ALWAYS AS IDENTITY is the modern replacement for SERIAL.
--
-- ⚠️  WARNING: This migration acquires an ACCESS EXCLUSIVE lock on all three tables
-- for the duration of the ALTER. On a live database with heavy traffic, schedule
-- this during a maintenance window or use pg_repack for a zero-downtime approach.
--
-- ⚠️  REQUIRES Go struct update: db.User, db.Transfer, db.Notification fields
-- must change from int32 → int64 after this migration is applied.

-- users
ALTER TABLE users
    ALTER COLUMN id         TYPE BIGINT,
    ALTER COLUMN id         SET DEFAULT nextval('users_id_seq'),
    ALTER COLUMN balance    TYPE BIGINT;

ALTER SEQUENCE users_id_seq AS BIGINT;

-- transfers
ALTER TABLE transfers
    ALTER COLUMN id        TYPE BIGINT,
    ALTER COLUMN id        SET DEFAULT nextval('transfers_id_seq'),
    ALTER COLUMN from_user TYPE BIGINT,
    ALTER COLUMN to_user   TYPE BIGINT,
    ALTER COLUMN amount    TYPE BIGINT;

ALTER SEQUENCE transfers_id_seq AS BIGINT;

-- notifications
ALTER TABLE notifications
    ALTER COLUMN id      TYPE BIGINT,
    ALTER COLUMN id      SET DEFAULT nextval('notifications_id_seq'),
    ALTER COLUMN user_id TYPE BIGINT;

ALTER SEQUENCE notifications_id_seq AS BIGINT;
