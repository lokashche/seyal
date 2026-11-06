-- +goose Up

-- Phone notes, the sender (Phase 4, part 4a-5, D90). The reader (00026) already keeps one notices row
-- per person and event; now a sender posts each phone note to the person's devices, through their
-- push services. These are still operational columns: phone notes are not ledger events (D85,
-- decision 4), and `seyal verify` doesn't read them.
--
--   next_try_at: no device took the phone note (the push service had a problem, asked Seyal to wait,
--                or Seyal couldn't reach it), so it waits until then: 1, 5 and 30 minutes after the
--                first, second and third try. After the fourth try it is given up ("failed").
--   result:      two more ways a notice ends:
--                  no_device: the person has no device with phone notes on (nothing was sent);
--                  failed:    no device took it, and it won't be tried again.
ALTER TABLE notices ADD COLUMN next_try_at timestamptz;
ALTER TABLE notices DROP CONSTRAINT notices_result_check;
ALTER TABLE notices ADD CONSTRAINT notices_result_check
    CHECK (result IN ('sent', 'seen', 'inactive', 'closed', 'decided', 'stale', 'no_device', 'failed'));
GRANT UPDATE (next_try_at) ON notices TO seyal_app;

-- +goose Down
UPDATE notices SET result = 'stale' WHERE result IN ('no_device', 'failed');
ALTER TABLE notices DROP CONSTRAINT notices_result_check;
ALTER TABLE notices ADD CONSTRAINT notices_result_check
    CHECK (result IN ('sent', 'seen', 'inactive', 'closed', 'decided', 'stale'));
ALTER TABLE notices DROP COLUMN next_try_at;
