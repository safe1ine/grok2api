CREATE INDEX IF NOT EXISTS idx_minute_usage_stats_key_minute
    ON minute_usage_stats (key_id, minute);
