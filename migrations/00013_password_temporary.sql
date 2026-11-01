-- +goose Up

-- A password made by an admin (or by `seyal set-password`) is temporary: whoever logs in with it
-- chooses their own before doing anything else, so the admin never knows a password in use.
ALTER TABLE persons ADD COLUMN password_temporary boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE persons DROP COLUMN password_temporary;
