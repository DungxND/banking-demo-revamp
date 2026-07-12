-- Migration 000002: banking invariants + missing FK indexes
--
-- Postgres skill findings applied:
--
-- 1. balance CHECK (>= 0) — the database enforces no negative balance as a
--    last-resort guard even if application logic is wrong.
--
-- 2. transfers.amount CHECK (> 0) — a zero or negative transfer is always a bug.
--
-- 3. Indexes on transfers.from_user and transfers.to_user — PostgreSQL does NOT
--    auto-create indexes for foreign key columns. Without them every query that
--    filters by from_user or to_user (e.g. a user's transfer history) performs a
--    full table scan. These are the hottest read columns in the transfers table.
--
-- 4. Composite index transfers(from_user, created_at DESC) — supports the common
--    "show my recent transfers" query pattern with a single index-only scan.
--    Covers ORDER BY created_at DESC queries filtered by a single user.

-- Guard against negative balances at the database level.
ALTER TABLE users
    ADD CONSTRAINT users_balance_non_negative CHECK (balance >= 0);

-- Guard against zero/negative transfer amounts at the database level.
ALTER TABLE transfers
    ADD CONSTRAINT transfers_amount_positive CHECK (amount > 0);

-- Missing FK indexes on transfers (high-traffic join/filter columns).
CREATE INDEX IF NOT EXISTS idx_transfers_from_user
    ON transfers (from_user);

CREATE INDEX IF NOT EXISTS idx_transfers_to_user
    ON transfers (to_user);

-- Composite covering index: user's transfers sorted by recency.
-- Supports: WHERE from_user = $1 ORDER BY created_at DESC LIMIT n
CREATE INDEX IF NOT EXISTS idx_transfers_from_user_created_at
    ON transfers (from_user, created_at DESC);

-- Composite index on notifications for the hot user-facing endpoint:
--   SELECT ... WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50
-- Without this index PostgreSQL uses idx_notifications_user_id to find
-- the rows then sorts them in memory. With (user_id, created_at DESC)
-- it satisfies both the WHERE filter AND the ORDER BY in a single
-- index scan — no heap sort, no extra work_mem allocation.
CREATE INDEX IF NOT EXISTS idx_notifications_user_id_created_at
    ON notifications (user_id, created_at DESC);
