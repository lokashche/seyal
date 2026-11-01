-- +goose Up

-- One row per AI call: what it was for, which model, the tokens it used and its estimated cost, so
-- a business can see what AI costs and choose models from real numbers. Operational records, not
-- part of the ledger: anything AI changes in a work item is still a ledger event of its own.
CREATE TABLE ai_calls (
    id            uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid          NOT NULL REFERENCES tenants (id),
    purpose       text          NOT NULL,
    model         text          NOT NULL, -- "provider:model"
    person_id     uuid,
    work_item_id  uuid,
    input_tokens  int           NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    cached_tokens int           NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
    cache_write_tokens int      NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    output_tokens int           NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_usd      numeric(12,6) NOT NULL DEFAULT 0,
    duration_ms   int           NOT NULL DEFAULT 0,
    ok            boolean       NOT NULL,
    error         text          NOT NULL DEFAULT '' CHECK (length(error) <= 500),
    created_at    timestamptz   NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, person_id)    REFERENCES persons (tenant_id, id),
    FOREIGN KEY (tenant_id, work_item_id) REFERENCES work_items (tenant_id, id)
);
CREATE INDEX ai_calls_tenant_time ON ai_calls (tenant_id, created_at);

ALTER TABLE ai_calls ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_calls FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_calls
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT ON ai_calls TO seyal_app;

-- +goose Down
DROP TABLE ai_calls;
