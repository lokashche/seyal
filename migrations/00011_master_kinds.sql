-- +goose Up

-- Each business's own lists (master kinds): what its clients, work types, members, courts... look
-- like. The definition is kept as text, like a process version. Changing one is an update here plus
-- a ledger event; nothing is deleted. Businesses that already have master data get their lists
-- copied from the templates the next time Seyal starts (the definitions live in files, not SQL).
CREATE TABLE master_kinds (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES tenants (id),
    kind           text        NOT NULL,
    label          text        NOT NULL,
    version        int         NOT NULL DEFAULT 1 CHECK (version >= 1),
    content_sha256 bytea       NOT NULL CHECK (length(content_sha256) = 32),
    source         text        NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, kind),
    UNIQUE (tenant_id, id)
);

ALTER TABLE master_kinds ENABLE ROW LEVEL SECURITY;
ALTER TABLE master_kinds FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON master_kinds
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT, UPDATE ON master_kinds TO seyal_app;

-- +goose Down
DROP TABLE master_kinds;
