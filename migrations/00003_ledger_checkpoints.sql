-- +goose Up

-- Points up to which a chain was verified, so the next check only needs to look at new events.
CREATE TABLE ledger_checkpoints (
    tenant_id   uuid        NOT NULL REFERENCES tenants (id),
    seq         bigint      NOT NULL,
    hash        bytea       NOT NULL CHECK (length(hash) = 32),
    verified_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, seq)
);

ALTER TABLE ledger_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_checkpoints FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger_checkpoints
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- +goose Down
DROP TABLE ledger_checkpoints;
