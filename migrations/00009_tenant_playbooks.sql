-- +goose Up

-- Which processes (playbooks) each business uses, and which version new work starts on. Changing
-- the version or switching a process off is an update here plus a ledger event; nothing is deleted.
CREATE TABLE tenant_playbooks (
    tenant_id          uuid        NOT NULL REFERENCES tenants (id),
    playbook_id        text        NOT NULL,
    current_version_id uuid        NOT NULL,
    active             boolean     NOT NULL DEFAULT true,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, playbook_id),
    FOREIGN KEY (tenant_id, current_version_id) REFERENCES playbook_versions (tenant_id, id)
);

ALTER TABLE tenant_playbooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_playbooks FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_playbooks
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT, UPDATE ON tenant_playbooks TO seyal_app;

-- Businesses that already use playbooks keep using the newest version they registered. Row-level
-- security applies to this migration too, so it goes business by business.
-- +goose StatementBegin
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        INSERT INTO tenant_playbooks (tenant_id, playbook_id, current_version_id)
        SELECT DISTINCT ON (playbook_id) tenant_id, playbook_id, id
        FROM playbook_versions
        WHERE tenant_id = t
        ORDER BY playbook_id, registered_at DESC;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE tenant_playbooks;
