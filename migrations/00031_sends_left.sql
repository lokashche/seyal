-- +goose Up

-- Leave it for a failed send (Phase 4 part 4c-3, D96): an admin may decide a send that failed or
-- expired should not go (the item was cancelled, or its other app was taken out of the set-up file).
-- left_at marks it; the send then no longer counts as failed in My Work or Leaves Seyal. The admin's
-- choice is an item.send_left ledger event, written in the same transaction.
ALTER TABLE sends ADD COLUMN left_at timestamptz;
ALTER TABLE sends ADD CONSTRAINT sends_left_only_failed
    CHECK (left_at IS NULL OR gave_up_at IS NOT NULL OR expired_at IS NOT NULL);
GRANT UPDATE (left_at) ON sends TO seyal_app;

-- +goose Down
ALTER TABLE sends DROP COLUMN left_at;
