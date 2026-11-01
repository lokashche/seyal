-- +goose Up

-- Roles are taken away by setting revoked_at, never by deleting the row, so the history of who
-- held which role stays. Giving the role back clears it again. Every role check ignores revoked
-- rows.
ALTER TABLE person_roles ADD COLUMN revoked_at timestamptz;
GRANT UPDATE (revoked_at) ON person_roles TO seyal_app;

-- +goose Down
REVOKE UPDATE (revoked_at) ON person_roles FROM seyal_app;
ALTER TABLE person_roles DROP COLUMN revoked_at;
