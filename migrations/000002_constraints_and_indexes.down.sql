DROP INDEX IF EXISTS idx_notifications_user_id_created_at;
DROP INDEX IF EXISTS idx_transfers_from_user_created_at;
DROP INDEX IF EXISTS idx_transfers_to_user;
DROP INDEX IF EXISTS idx_transfers_from_user;

ALTER TABLE transfers DROP CONSTRAINT IF EXISTS transfers_amount_positive;
ALTER TABLE users     DROP CONSTRAINT IF EXISTS users_balance_non_negative;
