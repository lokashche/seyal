-- +goose Up

-- Master-data items: one row per client, work type, and so on. The kinds and their fields come
-- from master-kind files. Items are never deleted; set active = false instead.
CREATE TABLE master_items (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id),
    kind       text        NOT NULL,
    key        text        NOT NULL CHECK (key ~ '^[a-z0-9][a-z0-9_-]*$'),
    name       text        NOT NULL,
    fields     jsonb       NOT NULL DEFAULT '{}',
    version    int         NOT NULL DEFAULT 1 CHECK (version > 0),
    active     boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, kind, key),
    UNIQUE (tenant_id, id)
);

-- Gap-free counters, e.g. for work item numbers. A number is taken inside the same transaction
-- that uses it, so a failed transaction gives its number back.
CREATE TABLE counters (
    tenant_id  uuid   NOT NULL REFERENCES tenants (id),
    key        text   NOT NULL, -- e.g. "job-evaluation:2026"
    next_value bigint NOT NULL DEFAULT 1 CHECK (next_value > 0),
    PRIMARY KEY (tenant_id, key)
);

-- Work items: the one core object. This table is the current picture; the ledger holds every
-- change that led to it.
CREATE TABLE work_items (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants (id),
    number              text        NOT NULL,
    playbook_version_id uuid        NOT NULL,
    title               text        NOT NULL DEFAULT '',
    state               text        NOT NULL,
    assignee_id         uuid,
    due_on              date,
    version             int         NOT NULL DEFAULT 1 CHECK (version > 0),
    fields              jsonb       NOT NULL DEFAULT '{}',
    fields_sha256       text        NOT NULL,
    started_via         text        NOT NULL CHECK (started_via IN ('chat', 'schedule', 'event')),
    started_ref         text,
    created_by          uuid,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, number),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, playbook_version_id) REFERENCES playbook_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, assignee_id)         REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by)          REFERENCES persons (tenant_id, id)
);

-- Links between items. parent: from = parent, to = child.
-- started_by: from = the new item, to = the item whose change started it.
CREATE TABLE work_item_links (
    tenant_id    uuid        NOT NULL,
    from_item_id uuid        NOT NULL,
    to_item_id   uuid        NOT NULL,
    kind         text        NOT NULL CHECK (kind IN ('parent', 'started_by')),
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, from_item_id, to_item_id, kind),
    CHECK (from_item_id <> to_item_id),
    FOREIGN KEY (tenant_id, from_item_id) REFERENCES work_items (tenant_id, id),
    FOREIGN KEY (tenant_id, to_item_id)   REFERENCES work_items (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by)   REFERENCES persons (tenant_id, id)
);

-- Approval decisions, each bound to one item version and fields fingerprint. The database also
-- enforces "one decision per slot" and "one decision per person" for each version.
CREATE TABLE approvals (
    tenant_id     uuid        NOT NULL,
    work_item_id  uuid        NOT NULL,
    item_version  int         NOT NULL,
    fields_sha256 text        NOT NULL,
    slot          text        NOT NULL,
    person_id     uuid        NOT NULL,
    decision      text        NOT NULL CHECK (decision IN ('approve', 'reject')),
    reason        text,
    decided_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, work_item_id, item_version, slot),
    UNIQUE (tenant_id, work_item_id, item_version, person_id),
    CHECK (decision = 'approve' OR coalesce(trim(reason), '') <> ''),
    FOREIGN KEY (tenant_id, work_item_id) REFERENCES work_items (tenant_id, id),
    FOREIGN KEY (tenant_id, person_id)    REFERENCES persons (tenant_id, id)
);

ALTER TABLE master_items    ENABLE ROW LEVEL SECURITY;
ALTER TABLE master_items    FORCE  ROW LEVEL SECURITY;
ALTER TABLE counters        ENABLE ROW LEVEL SECURITY;
ALTER TABLE counters        FORCE  ROW LEVEL SECURITY;
ALTER TABLE work_items      ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_items      FORCE  ROW LEVEL SECURITY;
ALTER TABLE work_item_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_item_links FORCE  ROW LEVEL SECURITY;
ALTER TABLE approvals       ENABLE ROW LEVEL SECURITY;
ALTER TABLE approvals       FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON master_items
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON counters
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON work_items
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON work_item_links
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON approvals
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Never DELETE. Links and decisions are never changed either.
GRANT SELECT, INSERT, UPDATE ON master_items, counters, work_items TO seyal_app;
GRANT SELECT, INSERT         ON work_item_links, approvals         TO seyal_app;

-- +goose Down
DROP TABLE approvals;
DROP TABLE work_item_links;
DROP TABLE work_items;
DROP TABLE counters;
DROP TABLE master_items;
