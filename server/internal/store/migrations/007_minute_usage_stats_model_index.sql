CREATE INDEX IF NOT EXISTS idx_minute_usage_stats_model
    ON minute_usage_stats (model_name)
    WHERE model_name <> '';
