CREATE TABLE IF NOT EXISTS app_migration_markers (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS minute_usage_stats (
    minute                         TIMESTAMPTZ NOT NULL,
    key_id                         BIGINT NOT NULL DEFAULT 0,
    model_name                     TEXT NOT NULL DEFAULT '',
    calls                          BIGINT NOT NULL DEFAULT 0,
    input_tokens                   BIGINT NOT NULL DEFAULT 0,
    cached_tokens                  BIGINT NOT NULL DEFAULT 0,
    output_tokens                  BIGINT NOT NULL DEFAULT 0,
    long_context_input_tokens      BIGINT NOT NULL DEFAULT 0,
    long_context_cached_tokens     BIGINT NOT NULL DEFAULT 0,
    long_context_output_tokens     BIGINT NOT NULL DEFAULT 0,
    updated_at                     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (minute, key_id, model_name)
);

DO $migration$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM app_migration_markers WHERE name = '004_minute_usage_stats_backfill'
    ) THEN
        INSERT INTO minute_usage_stats (
            minute, key_id, model_name, calls,
            input_tokens, cached_tokens, output_tokens,
            long_context_input_tokens, long_context_cached_tokens, long_context_output_tokens
        )
        SELECT
            date_trunc('minute', created_at),
            COALESCE(key_id, 0),
            COALESCE(model, ''),
            count(*)::BIGINT,
            COALESCE(sum(prompt_tokens), 0)::BIGINT,
            COALESCE(sum(cached_tokens), 0)::BIGINT,
            COALESCE(sum(completion_tokens), 0)::BIGINT,
            COALESCE(sum(CASE WHEN prompt_tokens > 200000 THEN prompt_tokens ELSE 0 END), 0)::BIGINT,
            COALESCE(sum(CASE WHEN prompt_tokens > 200000 THEN cached_tokens ELSE 0 END), 0)::BIGINT,
            COALESCE(sum(CASE WHEN prompt_tokens > 200000 THEN completion_tokens ELSE 0 END), 0)::BIGINT
        FROM call_logs
        GROUP BY
            date_trunc('minute', created_at),
            COALESCE(key_id, 0),
            COALESCE(model, '')
        ON CONFLICT (minute, key_id, model_name) DO UPDATE SET
            calls = EXCLUDED.calls,
            input_tokens = EXCLUDED.input_tokens,
            cached_tokens = EXCLUDED.cached_tokens,
            output_tokens = EXCLUDED.output_tokens,
            long_context_input_tokens = EXCLUDED.long_context_input_tokens,
            long_context_cached_tokens = EXCLUDED.long_context_cached_tokens,
            long_context_output_tokens = EXCLUDED.long_context_output_tokens,
            updated_at = now();

        INSERT INTO app_migration_markers (name)
        VALUES ('004_minute_usage_stats_backfill')
        ON CONFLICT (name) DO NOTHING;
    END IF;
END
$migration$;
