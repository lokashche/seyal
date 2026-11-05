-- +goose Up

-- One login across businesses, second half (D64): from now on a password lives only in its login
-- (accounts), and the app can no longer write one into persons. Must reach a live server in the
-- same release as 00015: in between, the old code still changed passwords in persons only.
--
-- Business by business, oldest first (row-level security binds the owner too):
--   1. people given a password since 00015 get a login, like in 00015;
--   2. a linked person whose own password or email changed since then (by the old code) no longer
--      matches their login, so they are unlinked; the carry-over then runs again, which gives one
--      whose email changed a login with the new email. An email that already has a login with
--      another hash (always so for a password chosen separately: each hash has its own salt) stays
--      unlinked: that login doesn't reach this business, and this business's own copy of the
--      password no longer works anywhere. They are added with a join link or `seyal link`;
--   3. linked people's old copies of their password are cleared.
-- The notice (printed by `seyal migrate`) gives only counts, never an email.
-- +goose StatementBegin
DO $$
DECLARE
    t        uuid;
    unlinked int := 0;
    left_out int := 0;
    n        int;
BEGIN
    FOR t IN SELECT id FROM tenants ORDER BY created_at, id LOOP
        PERFORM link_existing_logins(t);
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE persons p SET account_id = NULL
        FROM accounts a
        WHERE p.tenant_id = t AND p.account_id = a.id
          AND (p.login_email IS DISTINCT FROM a.email
               OR (p.password_hash IS NOT NULL AND p.password_hash <> a.password_hash));
        GET DIAGNOSTICS n = ROW_COUNT;
        unlinked := unlinked + n;
        left_out := left_out + link_existing_logins(t);
        UPDATE persons SET password_hash = NULL
        WHERE tenant_id = t AND account_id IS NOT NULL AND password_hash IS NOT NULL;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    RAISE NOTICE 'accounts_only: % person(s) unlinked (changed since 00015), % person(s) left without a login', unlinked, left_out;
END
$$;
-- +goose StatementEnd

-- The app changes only these columns of a person (people.go: name, email, status; auth.Link: the
-- login). Never a password or its "temporary" flag, nor a person's id or business.
REVOKE UPDATE ON persons FROM seyal_app;
GRANT UPDATE (display_name, login_email, status, account_id) ON persons TO seyal_app;

-- +goose Down
-- Revoking the table's UPDATE also takes away the column grants.
REVOKE UPDATE ON persons FROM seyal_app;
GRANT UPDATE ON persons TO seyal_app;

-- Linked people get their login's password back in persons, so the older code can check it. People
-- unlinked by the Up stay unlinked; they kept their own password.
-- +goose StatementBegin
DO $$
DECLARE
    t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants ORDER BY created_at, id LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE persons p SET password_hash = a.password_hash, password_temporary = a.password_temporary
        FROM accounts a
        WHERE p.tenant_id = t AND p.account_id = a.id AND p.password_hash IS NULL;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;
-- +goose StatementEnd
