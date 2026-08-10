CREATE TABLE IF NOT EXISTS user_password_credentials (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    login_normalized text NOT NULL UNIQUE,
    password_hash text NOT NULL CHECK (length(password_hash) BETWEEN 64 AND 512),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (login_normalized = lower(btrim(login_normalized))),
    CHECK (length(login_normalized) BETWEEN 1 AND 100)
);
