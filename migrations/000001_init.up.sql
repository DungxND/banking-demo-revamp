CREATE TABLE IF NOT EXISTS users (
    id             SERIAL PRIMARY KEY,
    phone          VARCHAR(20)  NOT NULL UNIQUE,
    account_number VARCHAR(20)  NOT NULL UNIQUE,
    username       VARCHAR(50)  NOT NULL,
    password_hash  VARCHAR(255) NOT NULL,
    balance        INTEGER      NOT NULL DEFAULT 100000,
    is_admin       BOOLEAN      NOT NULL DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_users_phone          ON users (phone);
CREATE INDEX IF NOT EXISTS idx_users_account_number ON users (account_number);
CREATE INDEX IF NOT EXISTS idx_users_username       ON users (username);

CREATE TABLE IF NOT EXISTS transfers (
    id         SERIAL PRIMARY KEY,
    from_user  INTEGER NOT NULL REFERENCES users (id),
    to_user    INTEGER NOT NULL REFERENCES users (id),
    amount     INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS notifications (
    id         SERIAL PRIMARY KEY,
    user_id    INTEGER      NOT NULL REFERENCES users (id),
    message    VARCHAR(255) NOT NULL,
    is_read    BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_notifications_user_id ON notifications (user_id);
