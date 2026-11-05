-- +goose Up

-- Moving work between people (D68, D70). Each item keeps its latest hand-over: when the person or team
-- that has it now got it (held_since), from whom (handoff_from, a person) and the note given with it.
-- Like every work item column, they are in each ledger snapshot, so `seyal verify` checks them. Old
-- items are not filled in: they show no "since" and no "From ..." hint until they are handed over again.
ALTER TABLE work_items ADD COLUMN held_since timestamptz;
ALTER TABLE work_items ADD COLUMN handoff_from uuid;
ALTER TABLE work_items ADD COLUMN handoff_note text;
-- Two columns, like assignee_id: a person from another business is refused.
ALTER TABLE work_items ADD FOREIGN KEY (tenant_id, handoff_from) REFERENCES persons (tenant_id, id);

-- One item's events, found fast (who had a step last, the last question Seyal asked).
CREATE INDEX ledger_subject ON ledger (tenant_id, subject_id);

-- +goose Down
DROP INDEX ledger_subject;
ALTER TABLE work_items DROP COLUMN handoff_note;
ALTER TABLE work_items DROP COLUMN handoff_from;
ALTER TABLE work_items DROP COLUMN held_since;
