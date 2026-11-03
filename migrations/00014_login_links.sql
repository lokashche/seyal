-- +goose Up

-- One-time links to set a password: an admin makes one for a new person (or one who forgot their
-- password) and sends it to them, e.g. on WhatsApp. Like sessions, the address holds a random code
-- and only its SHA-256 is stored, so a copy of this table can't be used. A link works once, until it
-- expires; a newer link for the same person cancels the older ones. Never deleted.
CREATE TABLE login_links (
    code_sha256  text        PRIMARY KEY CHECK (code_sha256 ~ '^[0-9a-f]{64}$'),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id),
    person_id    uuid        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    cancelled_at timestamptz,
    FOREIGN KEY (tenant_id, person_id) REFERENCES persons (tenant_id, id)
);
CREATE INDEX login_links_person ON login_links (tenant_id, person_id);

ALTER TABLE login_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE login_links FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON login_links
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT ON login_links TO seyal_app;
GRANT UPDATE (used_at, cancelled_at) ON login_links TO seyal_app;

-- +goose Down
DROP TABLE login_links;
