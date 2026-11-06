-- +goose Up

-- The chaser (Phase 2, D75, D76). Each item keeps its current step stay (one stay in one step, from
-- when it came in until it leaves): when it started (step_since), the last reminder in it (reminded_at),
-- how many reminders it had (reminders, 0 to 2) and when it was escalated (escalated_at). What Seyal did
-- is kept on the item itself, so it is in every ledger snapshot and `seyal verify` checks it, and Seyal
-- never does the same thing twice, even after a restart. Starting a new stay sets step_since and clears
-- the other three. Old items are not filled in: an empty step_since means "never chase", and they are on
-- process versions without timing anyway.
ALTER TABLE work_items ADD COLUMN step_since timestamptz;
ALTER TABLE work_items ADD COLUMN reminded_at timestamptz;
ALTER TABLE work_items ADD COLUMN reminders smallint NOT NULL DEFAULT 0 CHECK (reminders BETWEEN 0 AND 2);
ALTER TABLE work_items ADD COLUMN escalated_at timestamptz;

-- +goose Down
ALTER TABLE work_items DROP COLUMN escalated_at;
ALTER TABLE work_items DROP COLUMN reminders;
ALTER TABLE work_items DROP COLUMN reminded_at;
ALTER TABLE work_items DROP COLUMN step_since;
