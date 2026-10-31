-- +goose Up

-- Login sessions. The cookie holds a random token; only its SHA-256 is stored, so a copy of this
-- table can't be used to log in. Sessions are never deleted: logging out sets ended_at.
CREATE TABLE sessions (
    token_sha256 text        PRIMARY KEY CHECK (token_sha256 ~ '^[0-9a-f]{64}$'),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id),
    person_id    uuid        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    ended_at     timestamptz,
    user_agent   text        NOT NULL DEFAULT '' CHECK (length(user_agent) <= 500),
    FOREIGN KEY (tenant_id, person_id) REFERENCES persons (tenant_id, id)
);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sessions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT ON sessions TO seyal_app;
GRANT UPDATE (ended_at) ON sessions TO seyal_app;

-- +goose Down
DROP TABLE sessions;
