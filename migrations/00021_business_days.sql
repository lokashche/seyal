-- +goose Up

-- Deadlines that act (Phase 2, D75, D77). Seyal counts waits in working days: each business lists its
-- weekly days off (0 = Sunday ... 6 = Saturday) and, if it wants, holiday dates. A business can also
-- pause Seyal's chasing for itself only. All three come from the business's set-up file, like the time
-- zone; the app role can only read this table. Businesses that already exist start with no days off,
-- no holidays and chasing on, so every day is a working day until their set-up file says otherwise.
ALTER TABLE tenants ADD COLUMN days_off smallint[] NOT NULL DEFAULT '{}'
    CHECK (days_off <@ '{0,1,2,3,4,5,6}'::smallint[] AND cardinality(days_off) < 7);
ALTER TABLE tenants ADD COLUMN holidays date[] NOT NULL DEFAULT '{}' CHECK (cardinality(holidays) <= 100);
ALTER TABLE tenants ADD COLUMN chase_paused boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE tenants DROP COLUMN chase_paused;
ALTER TABLE tenants DROP COLUMN holidays;
ALTER TABLE tenants DROP COLUMN days_off;
