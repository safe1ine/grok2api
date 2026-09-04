ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS accounts_scheduling_weight_check;

ALTER TABLE accounts
    ADD CONSTRAINT accounts_scheduling_weight_check
    CHECK (scheduling_weight BETWEEN 1 AND 1000);
