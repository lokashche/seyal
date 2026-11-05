-- +goose Up

-- Speak instead of type (D66): a voice note is turned into words, the person checks them and sends
-- the message. Nothing is kept until then: the browser holds the recording, and when the person
-- presses Send, the recording, the words the speech service heard and the final text are saved
-- together, in the same transaction as the message. Each row is what was heard in one sent
-- recording: the "raw" words exactly as the speech service gave them, and the words the AI tidy-up
-- made of them (empty when it wasn't used). The final text is the message's own text. Never edited.
CREATE TABLE transcripts (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id),
    media_id   uuid        NOT NULL,            -- the recording
    model      text        NOT NULL,            -- the speech service, "provider:model"
    language   text        NOT NULL DEFAULT '', -- as the service heard it, if it says
    raw        text        NOT NULL CHECK (length(raw) <= 10000),
    tidied     text        NOT NULL DEFAULT '' CHECK (length(tidied) <= 10000),
    tidy_model text        NOT NULL DEFAULT '', -- the tidy-up's model, "provider:model"; '' = not tidied
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, media_id) REFERENCES media (tenant_id, id)
);

ALTER TABLE transcripts ENABLE ROW LEVEL SECURITY;
ALTER TABLE transcripts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON transcripts
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

GRANT SELECT, INSERT ON transcripts TO seyal_app;

-- The message a transcript went into; its text is the final words the person sent.
ALTER TABLE messages ADD COLUMN transcript_id uuid;
ALTER TABLE messages ADD FOREIGN KEY (tenant_id, transcript_id) REFERENCES transcripts (tenant_id, id);
CREATE UNIQUE INDEX messages_transcript ON messages (tenant_id, transcript_id) WHERE transcript_id IS NOT NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN transcript_id;
DROP TABLE transcripts;
