ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS scheduling_weight INTEGER NOT NULL DEFAULT 1;

UPDATE accounts
SET scheduling_weight = LEAST(100, GREATEST(1, scheduling_weight));

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'accounts_scheduling_weight_check'
          AND conrelid = 'accounts'::regclass
    ) THEN
        ALTER TABLE accounts
            ADD CONSTRAINT accounts_scheduling_weight_check
            CHECK (scheduling_weight BETWEEN 1 AND 100);
    END IF;
END
$$;
