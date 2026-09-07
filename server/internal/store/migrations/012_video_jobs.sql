CREATE TABLE IF NOT EXISTS video_jobs (
    job_id     TEXT PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_video_jobs_expires_at ON video_jobs (expires_at);

DELETE FROM video_jobs WHERE expires_at <= now();
