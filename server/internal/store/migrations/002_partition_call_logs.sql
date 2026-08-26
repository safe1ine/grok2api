BEGIN;

DO $migration$
DECLARE
    min_ts          TIMESTAMPTZ;
    max_ts          TIMESTAMPTZ;
    start_day       DATE;
    last_day        DATE;
    partition_day   DATE;
    partition_start TIMESTAMPTZ;
    partition_end   TIMESTAMPTZ;
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_partitioned_table pt
        JOIN pg_class c ON c.oid = pt.partrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = current_schema() AND c.relname = 'call_logs'
    ) THEN
        ALTER TABLE call_logs RENAME TO call_logs_legacy_002;

        CREATE TABLE call_logs (
            id                BIGINT NOT NULL DEFAULT nextval('call_logs_id_seq'),
            key_id            BIGINT REFERENCES api_keys(id) ON DELETE SET NULL,
            account_id        BIGINT REFERENCES accounts(id) ON DELETE SET NULL,
            model             TEXT,
            endpoint          TEXT,
            status            INT,
            prompt_tokens     INT NOT NULL DEFAULT 0,
            completion_tokens INT NOT NULL DEFAULT 0,
            latency_ms        INT NOT NULL DEFAULT 0,
            created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
            CONSTRAINT call_logs_partitioned_pkey PRIMARY KEY (created_at, id)
        ) PARTITION BY RANGE (created_at);

        SELECT COALESCE(min(created_at), now()), COALESCE(max(created_at), now())
        INTO min_ts, max_ts
        FROM call_logs_legacy_002;

        start_day := (LEAST(min_ts, now()) AT TIME ZONE 'Asia/Shanghai')::DATE;
        last_day := (GREATEST(max_ts, now() + INTERVAL '7 days') AT TIME ZONE 'Asia/Shanghai')::DATE;
        partition_day := start_day;

        WHILE partition_day <= last_day LOOP
            partition_start := partition_day::TIMESTAMP AT TIME ZONE 'Asia/Shanghai';
            partition_end := (partition_day + 1)::TIMESTAMP AT TIME ZONE 'Asia/Shanghai';
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF call_logs FOR VALUES FROM (%L) TO (%L)',
                'call_logs_' || to_char(partition_day, 'YYYYMMDD'),
                partition_start,
                partition_end
            );
            partition_day := partition_day + 1;
        END LOOP;

        INSERT INTO call_logs (
            id, key_id, account_id, model, endpoint, status,
            prompt_tokens, completion_tokens, latency_ms, created_at
        )
        SELECT
            id, key_id, account_id, model, endpoint, status,
            prompt_tokens, completion_tokens, latency_ms, created_at
        FROM call_logs_legacy_002;

        PERFORM setval(
            'call_logs_id_seq',
            COALESCE((SELECT max(id) FROM call_logs), 1),
            EXISTS (SELECT 1 FROM call_logs)
        );
        ALTER SEQUENCE call_logs_id_seq OWNED BY call_logs.id;
        DROP TABLE call_logs_legacy_002;
    END IF;
END
$migration$;

CREATE INDEX IF NOT EXISTS idx_call_logs_id_desc ON call_logs (id DESC);
CREATE INDEX IF NOT EXISTS idx_call_logs_created_at ON call_logs (created_at DESC);

COMMIT;
