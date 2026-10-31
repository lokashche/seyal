-- +goose Up

-- Every playbook version a business has used, with an exact copy of the file. Rows are never
-- changed: new content means a new version.
CREATE TABLE playbook_versions (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES tenants (id),
    playbook_id    text        NOT NULL,
    version        text        NOT NULL,
    content_sha256 bytea       NOT NULL CHECK (length(content_sha256) = 32),
    source         text        NOT NULL,
    registered_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, playbook_id, version),
    UNIQUE (tenant_id, id)
);

ALTER TABLE playbook_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE playbook_versions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON playbook_versions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT ON playbook_versions TO seyal_app;

-- +goose Down
DROP TABLE playbook_versions;
