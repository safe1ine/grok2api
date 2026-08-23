CREATE TABLE IF NOT EXISTS accounts (
    id                BIGSERIAL PRIMARY KEY,
    email             TEXT UNIQUE,
    subject           TEXT,
    refresh_token_enc BYTEA NOT NULL,
    status            TEXT NOT NULL DEFAULT 'active',
    cooldown_until    TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at      TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS api_keys (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    prefix     TEXT NOT NULL,
    revoked    BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS oauth_states (
    state         TEXT PRIMARY KEY,
    code_verifier TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS call_logs (
    id                BIGSERIAL PRIMARY KEY,
    key_id            BIGINT REFERENCES api_keys(id) ON DELETE SET NULL,
    account_id        BIGINT REFERENCES accounts(id) ON DELETE SET NULL,
    model             TEXT,
    endpoint          TEXT,
    status            INT,
    prompt_tokens     INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    latency_ms        INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_call_logs_created_at ON call_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts (status);
