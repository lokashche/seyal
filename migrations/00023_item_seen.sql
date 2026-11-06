-- +goose Up

-- The New mark (Phase 2, D75, M7). One row per person and item: the last time that person opened the
-- item or did something to it. A card is New for someone when the item came to them (or their team)
-- after that. Seen times drive only the person's own marks, so they are not work and not in the
-- ledger or `seyal verify`, on purpose. Two-column foreign keys: a row can't point at a person or an
-- item of another business.
CREATE TABLE item_seen (
    tenant_id uuid        NOT NULL,
    person_id uuid        NOT NULL,
    item_id   uuid        NOT NULL,
    seen_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, person_id, item_id),
    FOREIGN KEY (tenant_id, person_id) REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id)   REFERENCES work_items (tenant_id, id)
);

ALTER TABLE item_seen ENABLE ROW LEVEL SECURITY;
ALTER TABLE item_seen FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON item_seen
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Never DELETE: a newer time replaces the old one.
GRANT SELECT, INSERT, UPDATE ON item_seen TO seyal_app;

-- The first day: without rows every old item would look New. Each person of business t has "seen"
-- every item now: at the database's now, or at the item's latest hand-over or step time if that is
-- later (those come from the app's clock, which may run a little ahead). Only items with one of those
-- times can ever be New, so only those get rows.
-- (Every person, not only whoever has the item: approvers and people who may take free work see
-- cards too, and SQL can't tell from the process files who they are.) Row-level security applies to
-- the owner too, so this runs one business at a time. Only the owner may call it (tests do).
-- +goose StatementBegin
CREATE FUNCTION mark_all_seen(t uuid) RETURNS int LANGUAGE plpgsql AS $$
DECLARE
    prev text := coalesce(current_setting('app.tenant_id', true), '');
    n int;
BEGIN
    PERFORM set_config('app.tenant_id', t::text, true);
    INSERT INTO item_seen (tenant_id, person_id, item_id, seen_at)
    SELECT t, p.id, w.id, greatest(now(), w.held_since, w.step_since)
    FROM persons p JOIN work_items w ON w.tenant_id = p.tenant_id
    WHERE p.tenant_id = t AND (w.held_since IS NOT NULL OR w.step_since IS NOT NULL)
    ON CONFLICT (tenant_id, person_id, item_id) DO UPDATE SET seen_at = greatest(item_seen.seen_at, excluded.seen_at);
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM set_config('app.tenant_id', prev, true);
    RETURN n;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION mark_all_seen(uuid) FROM PUBLIC;

-- +goose StatementBegin
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM mark_all_seen(t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION mark_all_seen(uuid);
DROP TABLE item_seen;
