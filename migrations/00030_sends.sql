-- +goose Up

-- Sends to other apps (Phase 4, part 4c-3, D96): the outbox. A process's `do: send` writes one row
-- here, in the same transaction as the move that enters the sending state and its item.send_queued
-- ledger event, with the exact body frozen at that moment. There is no network call there: the sender's
-- own loop (internal/outbox) posts it later, outside any transaction, and records what the other app
-- answered (item.send_done or item.send_failed, written with the row's change).
--
--   id:          the send id ("snd_<business>_<n>"), the same on every try and on Send again, so the
--                other app can skip a repeat (it is the webhook-id header);
--   body:        the exact bytes posted (before the signature), kept as text, not jsonb, so their
--                SHA-256 stays the one the ledger recorded (jsonb would reorder the keys);
--   queued_at:   when it was queued (or queued again by an admin's Send again); a send not gone within
--                24 hours of it expires;
--   tries, next_at, last_try_at, last_status, last_reason: each try, kept only here (operational,
--                like ai_calls); last_reason is one of the safe caller's fixed short reasons, never Go's
--                error text or the address;
--   done_at, gave_up_at, expired_at: how it ended. Send again clears gave_up_at and expired_at.
--
-- `seyal verify` checks every item.send_queued event against its row (the body's SHA-256), and that
-- how the row ended matches the send's last event (queued, done, failed, left). Rows are never deleted.
CREATE TABLE sends (
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    id            text        NOT NULL CHECK (id ~ '^snd_[a-z0-9-]+_[0-9]+$' AND length(id) <= 200),
    item_id       uuid        NOT NULL,
    target        text        NOT NULL CHECK (target ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    body          text        NOT NULL CHECK (length(body) <= 65536),
    body_sha256   text        NOT NULL CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    item_version  int         NOT NULL,
    fields_sha256 text        NOT NULL,
    queued_at     timestamptz NOT NULL,
    tries         int         NOT NULL DEFAULT 0 CHECK (tries >= 0),
    next_at       timestamptz NOT NULL,
    last_try_at   timestamptz,
    last_status   int,
    last_reason   text CHECK (length(last_reason) <= 200),
    done_at       timestamptz,
    gave_up_at    timestamptz,
    expired_at    timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES work_items (tenant_id, id),
    CHECK (num_nonnulls(done_at, gave_up_at, expired_at) <= 1)
);
CREATE INDEX sends_waiting ON sends (tenant_id, next_at)
    WHERE done_at IS NULL AND gave_up_at IS NULL AND expired_at IS NULL;
CREATE INDEX sends_item ON sends (tenant_id, item_id);

ALTER TABLE sends ENABLE ROW LEVEL SECURITY;
ALTER TABLE sends FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sends
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT ON sends TO seyal_app;
GRANT UPDATE (queued_at, tries, next_at, last_try_at, last_status, last_reason, done_at, gave_up_at, expired_at)
    ON sends TO seyal_app;

-- +goose Down
DROP TABLE sends;
