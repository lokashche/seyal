-- +goose Up

-- A step can be owned by a role ("anyone in the team") instead of one person. team holds that role
-- while the item has no single assignee. Like every work item column, it is in each ledger snapshot.
ALTER TABLE work_items ADD COLUMN team text;

-- +goose Down
ALTER TABLE work_items DROP COLUMN team;
