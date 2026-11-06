-- +goose Up

-- Phone notes on an iPhone with one Home Screen icon per business (Phase 4, part 4a-2, D87). Each
-- icon is one business's own app (the manifest's scope is /t/{slug}/), with its own service worker
-- and its own device push address. A person in two businesses who turns notes on in both icons has
-- two rows; without this column the sender's look-up gave both rows to each business, so the same
-- note showed twice on that phone, and the copy under the other icon opened logged out in Safari.
-- home_tenant is set only for such an icon (the page says so when it turns notes on); NULL (any
-- other browser) still serves every business of the login (D64). It is a separate migration because
-- 00024 had already run on development databases.
ALTER TABLE push_devices ADD COLUMN home_tenant uuid REFERENCES tenants (id);

-- The sender's look-up (00024), now also skipping an iPhone icon of another business.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION push_devices_here()
    RETURNS TABLE (person_id uuid, device_id uuid, endpoint text, p256dh text, auth text, origin text)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
    SELECT p.id, d.id, d.endpoint, d.p256dh, d.auth, d.origin
    FROM public.persons p JOIN public.push_devices d ON d.account_id = p.account_id
    WHERE p.tenant_id = public.current_tenant() AND p.status = 'active'
      AND d.ended_at IS NULL AND d.last_opened_at > now() - interval '30 days'
      AND (d.home_tenant IS NULL OR d.home_tenant = p.tenant_id)
    ORDER BY p.id, d.created_at
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION push_devices_here()
    RETURNS TABLE (person_id uuid, device_id uuid, endpoint text, p256dh text, auth text, origin text)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
    SELECT p.id, d.id, d.endpoint, d.p256dh, d.auth, d.origin
    FROM public.persons p JOIN public.push_devices d ON d.account_id = p.account_id
    WHERE p.tenant_id = public.current_tenant() AND p.status = 'active'
      AND d.ended_at IS NULL AND d.last_opened_at > now() - interval '30 days'
    ORDER BY p.id, d.created_at
$$;
-- +goose StatementEnd
ALTER TABLE push_devices DROP COLUMN home_tenant;
