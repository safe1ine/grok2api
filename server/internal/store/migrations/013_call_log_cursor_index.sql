CREATE INDEX IF NOT EXISTS idx_call_logs_cursor
    ON call_logs (created_at DESC, id DESC);
