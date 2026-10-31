-- +goose Up

-- The current business for this transaction. Empty means "no business", so queries see nothing.
-- +goose StatementBegin
CREATE FUNCTION current_tenant() RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;
-- +goose StatementEnd

CREATE TABLE tenants (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    name       text NOT NULL,
    currency   text NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
    timezone   text NOT NULL DEFAULT 'UTC',
    fiscal_year_start_month int NOT NULL DEFAULT 1 CHECK (fiscal_year_start_month BETWEEN 1 AND 12),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE persons (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants (id),
    display_name  text NOT NULL,
    login_email   text CHECK (login_email = lower(login_email)),
    password_hash text,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, login_email)
);

CREATE TABLE roles (
    tenant_id uuid NOT NULL REFERENCES tenants (id),
    key       text NOT NULL,
    name      text NOT NULL,
    PRIMARY KEY (tenant_id, key)
);

-- Links include tenant_id, so a person can never get a role from another business.
CREATE TABLE person_roles (
    tenant_id uuid NOT NULL,
    person_id uuid NOT NULL,
    role_key  text NOT NULL,
    PRIMARY KEY (tenant_id, person_id, role_key),
    FOREIGN KEY (tenant_id, person_id) REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, role_key)  REFERENCES roles (tenant_id, key)
);

-- Row-level security: every query only sees rows of the current business.
ALTER TABLE persons      ENABLE ROW LEVEL SECURITY;
ALTER TABLE persons      FORCE  ROW LEVEL SECURITY;
ALTER TABLE roles        ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles        FORCE  ROW LEVEL SECURITY;
ALTER TABLE person_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE person_roles FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON persons
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON roles
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON person_roles
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- The app role may read and add, never delete.
GRANT USAGE ON SCHEMA public TO seyal_app;
GRANT SELECT ON tenants TO seyal_app;
GRANT SELECT, INSERT, UPDATE ON persons, roles TO seyal_app;
GRANT SELECT, INSERT ON person_roles TO seyal_app;

-- +goose Down
DROP TABLE person_roles;
DROP TABLE roles;
DROP TABLE persons;
DROP TABLE tenants;
DROP FUNCTION current_tenant();
