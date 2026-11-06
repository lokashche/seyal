-- +goose Up

-- One business's events of one kind, found fast. Seyal reads the names people have had (person.created,
-- person.renamed, person.updated) each time the AI reads a message, so it can leave Seyal's replies that
-- name someone out of what the model sees (D68, decision 18). Without this index Postgres would read every
-- event of the business each time, and the ledger only grows.
CREATE INDEX ledger_kind ON ledger (tenant_id, kind);

-- +goose Down
DROP INDEX ledger_kind;
