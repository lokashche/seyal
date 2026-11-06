-- +goose Up

-- Phone notes, the reader (Phase 4, part 4a-4, D89). Each minute the reader looks at each business's
-- new ledger events, works out who gets a phone note (internal/notify) and keeps one notices row per
-- person and event. Phone notes only point at events that are already in the ledger, so none of this
-- is a ledger event, and `seyal verify` doesn't read it (D85, decision 4). These are operational
-- tables, like ai_calls and item_seen. Rows are never deleted.

-- The business's own switch and hours, from its set-up file only (the app role can only read this
-- table), recorded in business.updated like days_off. Off by default (decision 28). Outside the hours,
-- and on days off and holidays, phone notes wait for the next working morning. "24:00" means "until
-- midnight". Times are written "09:00", so comparing them as text compares them as times.
ALTER TABLE tenants ADD COLUMN notifications boolean NOT NULL DEFAULT false;
ALTER TABLE tenants ADD COLUMN notify_from text NOT NULL DEFAULT '09:00'
    CHECK (notify_from ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$');
ALTER TABLE tenants ADD COLUMN notify_until text NOT NULL DEFAULT '19:00'
    CHECK (notify_until ~ '^(([01][0-9]|2[0-3]):[0-5][0-9]|24:00)$');
ALTER TABLE tenants ADD CONSTRAINT tenants_notify_hours CHECK (notify_from < notify_until);

-- Where the reader got to in each business's ledger (the last seq it read), and the admin's Pause
-- (Settings → Processes → Leaves Seyal). A new row starts at the ledger's end, so turning phone notes
-- on never tells anyone about old work. While phone notes are off or paused, last_seq still moves on.
-- Pressing Pause or Resume is a business.updated ledger event by the admin, written with this row.
CREATE TABLE notice_bookmark (
    tenant_id uuid        PRIMARY KEY REFERENCES tenants (id),
    last_seq  bigint      NOT NULL CHECK (last_seq >= 0),
    paused_at timestamptz,
    paused_by uuid,
    FOREIGN KEY (tenant_id, paused_by) REFERENCES persons (tenant_id, id),
    CHECK ((paused_at IS NULL) = (paused_by IS NULL))
);

ALTER TABLE notice_bookmark ENABLE ROW LEVEL SECURITY;
ALTER TABLE notice_bookmark FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notice_bookmark
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT ON notice_bookmark TO seyal_app;
GRANT UPDATE (last_seq, paused_at, paused_by) ON notice_bookmark TO seyal_app;

-- One phone note to make: who is told, what kind, and the ledger event it points at. It holds only
-- ids, the item's number and the process's own keys (a team's role, approval slots, a bulk move's
-- batch), never a title, a hand-over note or a value, so nothing private can reach a lock screen.
-- Reading the same event twice adds nothing (the unique key).
--   hold_until: made in quiet hours, so it waits for the next working morning (then it is part of
--               the morning note);
--   sent_at, note_id: when it went out, and the first notice of the phone note it went out in (a
--               grouped note carries several); the hourly cap counts phone notes by note_id;
--   result: what became of it: sent, or why it was dropped (the person opened the item since, is
--               no longer active here, the item is closed, the approval was given, or it waited too
--               long). Empty while it waits.
--   tries:  for the sender (part 4a-5).
CREATE TABLE notices (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   uuid        NOT NULL,
    person_id   uuid        NOT NULL,
    kind        text        NOT NULL CHECK (kind IN ('new_work', 'team_work', 'approve', 'approve_again', 'reminder', 'no_escalate')),
    item_id     uuid        NOT NULL,
    number      text        NOT NULL CHECK (length(number) <= 100),
    event_seq   bigint      NOT NULL,
    event_at    timestamptz NOT NULL,
    from_person uuid,
    team        text        NOT NULL DEFAULT '',
    slots       text[]      NOT NULL DEFAULT '{}',
    batch       text        NOT NULL DEFAULT '',
    made_at     timestamptz NOT NULL,
    hold_until  timestamptz,
    sent_at     timestamptz,
    note_id     bigint,
    tries       int         NOT NULL DEFAULT 0,
    result      text CHECK (result IN ('sent', 'seen', 'inactive', 'closed', 'decided', 'stale')),
    UNIQUE (tenant_id, person_id, event_seq, kind),
    FOREIGN KEY (tenant_id, person_id)   REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, from_person) REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id)     REFERENCES work_items (tenant_id, id),
    CHECK ((result = 'sent') = (sent_at IS NOT NULL AND note_id IS NOT NULL))
);
CREATE INDEX notices_waiting ON notices (tenant_id, person_id, id) WHERE result IS NULL;
CREATE INDEX notices_sent ON notices (tenant_id, person_id, sent_at) WHERE result = 'sent';

ALTER TABLE notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE notices FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notices
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT ON notices TO seyal_app;
GRANT UPDATE (hold_until, sent_at, note_id, tries, result) ON notices TO seyal_app;

-- +goose Down
DROP TABLE notices;
DROP TABLE notice_bookmark;
ALTER TABLE tenants DROP CONSTRAINT tenants_notify_hours;
ALTER TABLE tenants DROP COLUMN notify_until;
ALTER TABLE tenants DROP COLUMN notify_from;
ALTER TABLE tenants DROP COLUMN notifications;
