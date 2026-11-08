-- +goose Up

-- Other apps (Phase 4, part 4c-2, D95): the web addresses a business may send to (a Google Sheet's
-- Apps Script, n8n, Make, Zapier), from its private set-up file.
--
--   other_apps: written only by `seyal import` with the owner connection; the app may only read it.
--               url and secret are secrets: never shown, exported, logged or put in the ledger. Each
--               import that adds, changes or removes a row writes a business.updated event with the
--               row's SHA-256 (never the address or the secret), and `seyal verify` compares the two,
--               so a row changed outside Seyal is found.
--   other_app_state: what the app changes: an admin's Pause (why 'admin', a business.updated event by
--               the admin), Seyal's own pause (why 'gone': the address was switched off; why 'cap': too
--               many sends in an hour; part 4c-3), and the last test. Kept apart from other_apps, so an
--               import never undoes a pause. Operational, like ai_calls: verify doesn't read it.
CREATE TABLE other_apps (
    tenant_id   uuid    NOT NULL REFERENCES tenants (id),
    name        text    NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    label       text    NOT NULL CHECK (length(label) BETWEEN 1 AND 100),
    service     text    NOT NULL CHECK (length(service) BETWEEN 1 AND 60),
    url         text    NOT NULL CHECK (url LIKE 'https://%' AND length(url) <= 2048),
    redirect_to text[]  NOT NULL DEFAULT '{}',
    expect      text    NOT NULL CHECK (expect IN ('ok_json', '2xx')),
    fields      text[]  NOT NULL DEFAULT '{}',
    link        boolean NOT NULL DEFAULT false,
    secret      text    NOT NULL CHECK (secret LIKE 'whsec\_%'),
    paused      boolean NOT NULL DEFAULT false,
    PRIMARY KEY (tenant_id, name)
);

ALTER TABLE other_apps ENABLE ROW LEVEL SECURITY;
ALTER TABLE other_apps FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON other_apps
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT ON other_apps TO seyal_app;

CREATE TABLE other_app_state (
    tenant_id   uuid        NOT NULL REFERENCES tenants (id),
    name        text        NOT NULL,
    paused_at   timestamptz,
    paused_by   uuid,
    why         text CHECK (why IN ('admin', 'gone', 'cap')),
    tested_at   timestamptz,
    tested_by   uuid,
    test_result text CHECK (length(test_result) <= 200),
    PRIMARY KEY (tenant_id, name),
    FOREIGN KEY (tenant_id, paused_by) REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, tested_by) REFERENCES persons (tenant_id, id),
    CHECK ((paused_at IS NULL) = (why IS NULL)),
    CHECK ((why = 'admin') = (paused_by IS NOT NULL)),
    CHECK ((tested_at IS NULL) = (test_result IS NULL))
);

ALTER TABLE other_app_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE other_app_state FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON other_app_state
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT ON other_app_state TO seyal_app;
GRANT UPDATE (paused_at, paused_by, why, tested_at, tested_by, test_result) ON other_app_state TO seyal_app;

-- +goose Down
DROP TABLE other_app_state;
DROP TABLE other_apps;
