CREATE TABLE IF NOT EXISTS fallback_config (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    openai_base_url TEXT NOT NULL DEFAULT '',
    openai_model    TEXT NOT NULL DEFAULT '',
    openai_key_enc  BYTEA,
    anthropic_base_url TEXT NOT NULL DEFAULT '',
    anthropic_model    TEXT NOT NULL DEFAULT '',
    anthropic_key_enc  BYTEA,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO fallback_config (id)
VALUES (1)
ON CONFLICT (id) DO NOTHING;
