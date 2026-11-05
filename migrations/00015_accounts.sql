-- +goose Up

-- One login (email + one password) for a person in any number of businesses (D64). Each business
-- keeps its own person; persons.account_id says which login reaches that person. Logins are found
-- before any business is known, so like tenants this table has no row-level security. Instead the
-- app may read every column except the password hash, and never reads a hash at all: it asks
-- account_check() below whether a password is right, and the database answers yes or no.
CREATE TABLE accounts (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email              text        NOT NULL UNIQUE CHECK (email = lower(email) AND strpos(email, '@') > 0),
    password_hash      text        NOT NULL,
    password_temporary boolean     NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- Column by column: never password_hash for reading, never email for changing, and no DELETE.
-- (So app code adds a login with a plain INSERT and catches the duplicate-email error; ON CONFLICT
-- would need to read the hash.)
GRANT SELECT (id, email, password_temporary, created_at) ON accounts TO seyal_app;
GRANT INSERT (email, password_hash, password_temporary) ON accounts TO seyal_app;
GRANT UPDATE (password_hash, password_temporary) ON accounts TO seyal_app;

-- Login sessions, the same shape as sessions (00007): the cookie holds a random token and only its
-- SHA-256 is stored. Never deleted: logging out sets ended_at.
CREATE TABLE account_sessions (
    token_sha256 text        PRIMARY KEY CHECK (token_sha256 ~ '^[0-9a-f]{64}$'),
    account_id   uuid        NOT NULL REFERENCES accounts (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    ended_at     timestamptz,
    user_agent   text        NOT NULL DEFAULT '' CHECK (length(user_agent) <= 500)
);
CREATE INDEX account_sessions_account ON account_sessions (account_id) WHERE ended_at IS NULL;

GRANT SELECT, INSERT ON account_sessions TO seyal_app;
GRANT UPDATE (ended_at) ON account_sessions TO seyal_app;

-- pgcrypto (part of PostgreSQL, "trusted", so the database's owner may add it) checks passwords
-- inside the database. Its crypt() reads the bcrypt hashes Go makes ($2a$10$...) and, like Go, uses
-- only a password's first 72 bytes.
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

-- The app's only way to test a password: is pw right for the login with email e? One row (the login,
-- whether it must choose a new password, ok), or no row for an unknown email. No function or grant
-- gives the app a hash, so even injected SQL can only try guesses one at a time, each costing a
-- bcrypt check, never copy the hashes away to crack elsewhere (D64).
-- Same time either way: an unknown email, or a hash that isn't bcrypt (crypt() would treat one as
-- an old DES hash), is checked against a dummy bcrypt hash of a thrown-away value and never matches.
-- It runs as the owner (SECURITY DEFINER), which works because accounts has no row-level security.
-- The fixed search_path (temporary tables last) and the public. names stop anyone from slipping in
-- a look-alike table or function.
-- The password must be a query parameter ($2), never part of the SQL text. Postgres's default log
-- settings then never write it: log_statement = none and log_min_duration_statement = -1 log no
-- statements, and a failed statement is logged (log_min_error_statement = error) without its
-- parameter values (log_parameter_max_length_on_error = 0). pgx sends parameters apart from the text.
-- +goose StatementBegin
CREATE FUNCTION account_check(e text, pw text)
    RETURNS TABLE (id uuid, password_temporary boolean, ok boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    dummy constant text := '$2a$10$fyV9D.nxS42Z8JB6WUxCFeecJ/GxWBzhdW0zO7Npwoo7JrjFE0dgC';
    a      record;
    known  boolean;
    bcrypt boolean;
    hash   text;
BEGIN
    SELECT x.id, x.password_hash, x.password_temporary INTO a FROM public.accounts x WHERE x.email = lower(e);
    known := FOUND;
    bcrypt := known AND a.password_hash ~ '^\$2a\$[0-9]{2}\$[./A-Za-z0-9]{53}$';
    hash := CASE WHEN bcrypt THEN a.password_hash ELSE dummy END;
    ok := public.crypt(pw, hash) = hash; -- on its own line, so it always runs
    ok := bcrypt AND coalesce(ok, false);
    IF known THEN
        id := a.id;
        password_temporary := a.password_temporary;
        RETURN NEXT;
    END IF;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION account_check(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION account_check(text, text) TO seyal_app;

-- Which login reaches this person; at most one person per login in each business. Empty means
-- "no login yet".
ALTER TABLE persons ADD COLUMN account_id uuid REFERENCES accounts (id);
ALTER TABLE persons ADD CONSTRAINT persons_one_per_account UNIQUE (tenant_id, account_id);
CREATE INDEX persons_account ON persons (account_id) WHERE account_id IS NOT NULL;

-- A link is made or removed, never quietly moved to another login in one step.
-- +goose StatementBegin
CREATE FUNCTION persons_account_not_moved() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.account_id IS NOT NULL AND NEW.account_id IS NOT NULL AND OLD.account_id <> NEW.account_id THEN
        RAISE EXCEPTION 'a person''s login can be removed, not moved to another login';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER persons_account_not_moved BEFORE UPDATE OF account_id ON persons
    FOR EACH ROW EXECUTE FUNCTION persons_account_not_moved();

-- A business "entry": this login session opened this business. It gives each business's ledger its
-- own logged-in and logged-out events, so one business never learns when someone used another.
-- An entry's token_sha256 is the hash of a random token nobody is ever given, so no cookie opens it.
-- At most one open entry per login session and business; an ended one stays, and opening the
-- business again (e.g. after being unlinked and linked again) adds a new one.
ALTER TABLE sessions ADD COLUMN account_session text REFERENCES account_sessions (token_sha256);
CREATE UNIQUE INDEX sessions_one_entry ON sessions (tenant_id, account_session) WHERE ended_at IS NULL;

-- The current login for this transaction. Empty means "no login".
-- +goose StatementBegin
CREATE FUNCTION current_account() RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('app.account_id', true), '')::uuid
$$;
-- +goose StatementEnd

-- Outside any business, a login may read its own people (one per business), e.g. to list its
-- businesses. Policies are ORed, so the "no business" guard keeps this rule out of the way inside a
-- business. Reading only: adding or changing people still needs tenant_isolation.
CREATE POLICY own_account ON persons FOR SELECT
    USING (current_tenant() IS NULL AND account_id IS NOT NULL AND account_id = current_account());

-- Carry-over: gives each active person who has a password a login with the same email and password,
-- and links them. If another (older) business already gave that email a login with a different
-- password, the person is left unlinked and keeps their own password; the result counts them.
-- Disabled people get no login (so they can't claim an email either); if re-enabled, they need a
-- set-your-password link.
-- Safe to run again. Row-level security binds the owner too, so it opens the one business it works
-- on. Owner only: the app may not run it.
-- +goose StatementBegin
CREATE FUNCTION link_existing_logins(t uuid) RETURNS int LANGUAGE plpgsql AS $$
DECLARE
    prev     text := coalesce(current_setting('app.tenant_id', true), '');
    p        record;
    a        uuid;
    unlinked int := 0;
BEGIN
    PERFORM set_config('app.tenant_id', t::text, true);
    FOR p IN SELECT id, login_email, password_hash, password_temporary FROM persons
             WHERE tenant_id = t AND status = 'active' AND account_id IS NULL
               AND login_email IS NOT NULL AND password_hash IS NOT NULL
             ORDER BY created_at, id
    LOOP
        INSERT INTO accounts (email, password_hash, password_temporary)
        VALUES (p.login_email, p.password_hash, p.password_temporary)
        ON CONFLICT (email) DO NOTHING
        RETURNING id INTO a;
        IF a IS NULL THEN -- the email has a login already: link only if it is this same password
            SELECT id INTO a FROM accounts WHERE email = p.login_email AND password_hash = p.password_hash;
        END IF;
        IF a IS NULL THEN
            unlinked := unlinked + 1;
        ELSE
            UPDATE persons SET account_id = a WHERE tenant_id = t AND id = p.id;
        END IF;
        a := NULL;
    END LOOP;
    PERFORM set_config('app.tenant_id', prev, true);
    RETURN unlinked;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION link_existing_logins(uuid) FROM PUBLIC;

-- Existing passwords carry over now, business by business, oldest first, so nobody has to run a
-- command. The notice gives only a count, never an email.
-- +goose StatementBegin
DO $$
DECLARE
    t uuid;
    n int := 0;
BEGIN
    FOR t IN SELECT id FROM tenants ORDER BY created_at, id LOOP
        n := n + link_existing_logins(t);
    END LOOP;
    RAISE NOTICE 'link_existing_logins: % login(s) left unlinked', n;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION link_existing_logins(uuid);
DROP POLICY own_account ON persons;
DROP FUNCTION current_account();
DROP INDEX sessions_one_entry;
ALTER TABLE sessions DROP COLUMN account_session;
DROP TRIGGER persons_account_not_moved ON persons;
DROP FUNCTION persons_account_not_moved();
DROP INDEX persons_account;
ALTER TABLE persons DROP CONSTRAINT persons_one_per_account;
ALTER TABLE persons DROP COLUMN account_id;
DROP FUNCTION account_check(text, text);
DROP EXTENSION IF EXISTS pgcrypto;
DROP TABLE account_sessions;
DROP TABLE accounts;
