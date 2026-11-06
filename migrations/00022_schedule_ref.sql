-- +goose Up

-- Recurring work (Phase 2, D75, R). A schedule starts one item per list entry and period, and the
-- item's started_ref says which: "<process>:<period>:<entry>", for example "monthly-return:m2026-11:acme".
-- This index lets only one item have each key, so two copies of the app, or a round after a restart,
-- can't start the same work twice. The key has no version in it, so a new version doesn't start it again
-- either, and a cancelled item keeps its key, so cancelled work isn't started again.
CREATE UNIQUE INDEX work_items_schedule_ref ON work_items (tenant_id, started_ref) WHERE started_via = 'schedule';

-- +goose Down
DROP INDEX work_items_schedule_ref;
