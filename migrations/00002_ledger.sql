-- +goose Up

-- Every event, per business, in order. Rows can be added but never changed or removed.
CREATE TABLE ledger (
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    seq             bigint      NOT NULL CHECK (seq > 0),
    occurred_at     timestamptz NOT NULL,
    kind            text        NOT NULL,
    subject_type    text        NOT NULL,
    subject_id      uuid,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'agent', 'system')),
    actor_person_id uuid,
    on_behalf_of    uuid,
    body            text        NOT NULL,
    prev_hash       bytea       NOT NULL CHECK (length(prev_hash) = 32),
    hash            bytea       NOT NULL CHECK (length(hash) = 32),
    PRIMARY KEY (tenant_id, seq),
    FOREIGN KEY (tenant_id, actor_person_id) REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, on_behalf_of)    REFERENCES persons (tenant_id, id)
);

-- The latest event of each business's chain. Locked while adding an event, so events
-- of one business are added strictly one after another.
CREATE TABLE ledger_heads (
    tenant_id uuid   PRIMARY KEY REFERENCES tenants (id),
    last_seq  bigint NOT NULL,
    last_hash bytea  NOT NULL CHECK (length(last_hash) = 32)
);

-- Belt and braces: even the owner role cannot change or remove events while this trigger
-- is on. (The hash chain catches anyone who turns it off.)
-- +goose StatementBegin
CREATE FUNCTION ledger_is_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the ledger is append-only: % is not allowed', TG_OP;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_no_update_delete BEFORE UPDATE OR DELETE ON ledger
    FOR EACH ROW EXECUTE FUNCTION ledger_is_append_only();
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON ledger
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_is_append_only();

ALTER TABLE ledger       ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger       FORCE  ROW LEVEL SECURITY;
ALTER TABLE ledger_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_heads FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON ledger
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON ledger_heads
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT ON ledger TO seyal_app;
GRANT SELECT, INSERT, UPDATE ON ledger_heads TO seyal_app;

-- +goose Down
DROP TABLE ledger_heads;
DROP TABLE ledger;
DROP FUNCTION ledger_is_append_only();
