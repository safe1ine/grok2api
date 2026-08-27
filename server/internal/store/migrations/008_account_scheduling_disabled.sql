ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS scheduling_disabled BOOLEAN NOT NULL DEFAULT false;

UPDATE accounts
SET scheduling_disabled = true,
    status = 'active',
    updated_at = now()
WHERE status = 'disabled';
