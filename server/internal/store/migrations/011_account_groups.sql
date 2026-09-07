CREATE TABLE IF NOT EXISTS account_groups (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_account_groups_one_default
    ON account_groups (is_default)
    WHERE is_default;

INSERT INTO account_groups (name, is_default)
SELECT '默认分组', true
WHERE NOT EXISTS (SELECT 1 FROM account_groups WHERE is_default)
ON CONFLICT (name) DO UPDATE SET is_default = true;

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS group_id BIGINT REFERENCES account_groups(id) ON DELETE RESTRICT;

UPDATE accounts
SET group_id = (SELECT id FROM account_groups WHERE is_default LIMIT 1)
WHERE group_id IS NULL;

ALTER TABLE accounts
    ALTER COLUMN group_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_accounts_group_id ON accounts (group_id);
