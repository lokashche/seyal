-- +goose Up

-- A person's ids on each channel, e.g. a phone number on a chat service. One person can have many.
-- (The app's own web chat doesn't need this: there the person is logged in.)
CREATE TABLE channel_identities (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants (id),
    person_id   uuid        NOT NULL,
    channel     text        NOT NULL,
    external_id text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, channel, external_id),
    FOREIGN KEY (tenant_id, person_id) REFERENCES persons (tenant_id, id)
);

-- Files people send: voice notes, photos, documents. The bytes live in the media store (on disk),
-- found by their SHA-256; this table records who sent what. Never deleted.
CREATE TABLE media (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id),
    uploaded_by  uuid        NOT NULL,
    content_type text        NOT NULL,
    size         bigint      NOT NULL CHECK (size > 0),
    sha256       text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    storage_key  text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, uploaded_by) REFERENCES persons (tenant_id, id)
);

-- Messages in one format for every channel. work_item_id is the thread; NULL means "not yet
-- filed" (e.g. a new instruction waiting to become a work item).
CREATE TABLE messages (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants (id),
    work_item_id        uuid,
    author_kind         text        NOT NULL CHECK (author_kind IN ('person', 'seyal')),
    author_person_id    uuid,
    channel             text        NOT NULL,
    channel_identity_id uuid,
    external_id         text,
    text                text        NOT NULL DEFAULT '' CHECK (length(text) <= 10000),
    sent_at             timestamptz NOT NULL,
    received_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, channel, external_id),
    CHECK ((author_kind = 'person') = (author_person_id IS NOT NULL)),
    FOREIGN KEY (tenant_id, work_item_id)     REFERENCES work_items (tenant_id, id),
    FOREIGN KEY (tenant_id, author_person_id) REFERENCES persons (tenant_id, id)
);

CREATE TABLE message_media (
    tenant_id  uuid NOT NULL,
    message_id uuid NOT NULL,
    media_id   uuid NOT NULL,
    position   int  NOT NULL CHECK (position >= 0),
    PRIMARY KEY (tenant_id, message_id, media_id),
    FOREIGN KEY (tenant_id, message_id) REFERENCES messages (tenant_id, id),
    FOREIGN KEY (tenant_id, media_id)   REFERENCES media (tenant_id, id)
);

ALTER TABLE channel_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_identities FORCE  ROW LEVEL SECURITY;
ALTER TABLE media              ENABLE ROW LEVEL SECURITY;
ALTER TABLE media              FORCE  ROW LEVEL SECURITY;
ALTER TABLE messages           ENABLE ROW LEVEL SECURITY;
ALTER TABLE messages           FORCE  ROW LEVEL SECURITY;
ALTER TABLE message_media      ENABLE ROW LEVEL SECURITY;
ALTER TABLE message_media      FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON channel_identities
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON media
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON messages
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON message_media
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Messages are never edited: the app may only file a message into a thread (one column).
GRANT SELECT, INSERT ON channel_identities, media, messages, message_media TO seyal_app;
GRANT UPDATE (work_item_id) ON messages TO seyal_app;

-- +goose Down
DROP TABLE message_media;
DROP TABLE messages;
DROP TABLE media;
DROP TABLE channel_identities;
