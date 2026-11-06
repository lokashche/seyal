-- +goose Up

-- Phone notes (Phase 4, part 4a-2, D87): one row per browser that turned phone notes on. The browser
-- hands Seyal its device push address (endpoint: a long random web address at its push service) and
-- two lock keys (p256dh, auth), so that later a phone note can be locked for that browser alone
-- (RFC 8291). The sender needs them as they are, so they can't be kept only as a hash the way
-- account_sessions keeps its tokens.
--
-- Server-wide, like account_sessions: one browser serves every business of a login (D64). A row is
-- never deleted: turning off, logging out and the other endings set ended_at and ended_why. Phone
-- notes are not work, so nothing here is in the ledger or in `seyal verify` (D85, decision 4).
CREATE TABLE push_devices (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id         uuid        NOT NULL REFERENCES accounts (id),
    -- The login session that turned it on, or last opened Seyal in this browser. Logging out of
    -- that session ends the device.
    account_session_id text        NOT NULL REFERENCES account_sessions (token_sha256),
    endpoint           text        NOT NULL CHECK (endpoint ~ '^https://' AND length(endpoint) <= 2048),
    p256dh             text        NOT NULL CHECK (p256dh ~ '^[A-Za-z0-9_-]{87}=?$'),
    auth               text        NOT NULL CHECK (auth ~ '^[A-Za-z0-9_-]{22}(==)?$'),
    -- The address the browser used (https://host), for the link in each phone note.
    origin             text        NOT NULL CHECK (origin ~ '^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$' AND length(origin) <= 300),
    label              text        NOT NULL DEFAULT '' CHECK (length(label) <= 60), -- "Chrome on Android"
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_opened_at     timestamptz NOT NULL DEFAULT now(),
    last_ok_at         timestamptz, -- the push service last took a phone note for it (part 4a-5)
    ended_at           timestamptz,
    ended_why          text CHECK (ended_why IN (
        'turned_off',            -- the person pressed Turn off
        'blocked',               -- the browser's permission was taken back
        'logged_out',            -- its login session logged out
        'logged_out_everywhere', -- Log out on all devices
        'password',              -- a new password (changed, reset, or set by the operator)
        'other_login',           -- another login turned phone notes on in the same browser
        'replaced',              -- the same browser came back with new lock keys
        'too_many',              -- a sixth device for the login ended the oldest
        'unused',                -- Seyal wasn't opened in this browser for 30 days
        'gone'                   -- the push service said the address is gone (part 4a-5)
    )),
    CHECK ((ended_at IS NULL) = (ended_why IS NULL))
);

-- A device push address is open for one row at a time across the whole server. An ended row stays as
-- history, so the same browser can be turned on again later.
CREATE UNIQUE INDEX push_devices_open_endpoint ON push_devices (endpoint) WHERE ended_at IS NULL;
CREATE INDEX push_devices_account ON push_devices (account_id, created_at) WHERE ended_at IS NULL;
CREATE INDEX push_devices_session ON push_devices (account_session_id) WHERE ended_at IS NULL;

-- The row rule: a transaction sees only the devices of its own login (app.account_id, db.WithAccount),
-- and a business's transaction (no login set) sees none, so one business can never read another
-- login's lock keys. The rule is switched on but not forced: it binds the app, while the two owner-run
-- functions below may look across logins, each for one narrow purpose.
ALTER TABLE push_devices ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_login ON push_devices
    USING (account_id = current_account()) WITH CHECK (account_id = current_account());

GRANT SELECT, INSERT ON push_devices TO seyal_app;
GRANT UPDATE (account_session_id, ended_at, ended_why, last_ok_at, last_opened_at) ON push_devices TO seyal_app;

-- A shared phone: the current login (app.account_id) turns phone notes on, or opens Seyal, in a
-- browser whose device push address is still open for another login. That other login's row ends,
-- so the phone stops showing the other person's work. Knowing the address is what allows it: it is
-- a long random secret that only that browser (and its push service) has. Returns how many ended.
-- +goose StatementBegin
CREATE FUNCTION push_device_take(e text) RETURNS int
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    n int;
BEGIN
    IF public.current_account() IS NULL THEN
        RAISE EXCEPTION 'push_device_take: no login is set';
    END IF;
    UPDATE public.push_devices SET ended_at = now(), ended_why = 'other_login'
    WHERE endpoint = e AND ended_at IS NULL AND account_id <> public.current_account();
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION push_device_take(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION push_device_take(text) TO seyal_app;

-- The sender's only way to the lock keys (part 4a-5): the open devices of the current business's
-- (app.tenant_id) active people, opened in the last 30 days. Nothing of a login that isn't an active
-- person here. persons' own row rule (forced, so it binds this owner-run function too) keeps the
-- look-up inside the current business; with no business set it returns nothing.
-- +goose StatementBegin
CREATE FUNCTION push_devices_here()
    RETURNS TABLE (person_id uuid, device_id uuid, endpoint text, p256dh text, auth text, origin text)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
    SELECT p.id, d.id, d.endpoint, d.p256dh, d.auth, d.origin
    FROM public.persons p JOIN public.push_devices d ON d.account_id = p.account_id
    WHERE p.tenant_id = public.current_tenant() AND p.status = 'active'
      AND d.ended_at IS NULL AND d.last_opened_at > now() - interval '30 days'
    ORDER BY p.id, d.created_at
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION push_devices_here() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION push_devices_here() TO seyal_app;

-- +goose Down
DROP FUNCTION push_devices_here();
DROP FUNCTION push_device_take(text);
DROP TABLE push_devices;
