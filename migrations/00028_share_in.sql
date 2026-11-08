-- +goose Up

-- Share in (Phase 4, part 4b-3, D93). A person may share a voice note, a photo, a PDF or words from
-- another app (WhatsApp on Android) into Seyal; they wait on the phone until the person presses Send.
--
--   messages.via: how a message came in. NULL is typed or spoken by the person, as before; 'share'
--                 is something shared into Seyal from another app. The words or the voice may be
--                 someone else's (a client's), so they never act (no move, no approval, no Yes), and
--                 they are kept away from the AI. Like the rest of a message it is written once and
--                 never changed (the app may only INSERT it), and `seyal verify` compares it with the
--                 message.received event.
--   tenants.sharing: the set-up file's "sharing: off" turns sharing off for one business: the Share
--                 panel is hidden and shared messages are refused. Written only by the set-up import
--                 (the owner connection), and recorded in business.updated, like days_off.
ALTER TABLE messages ADD COLUMN via text CHECK (via = 'share');
ALTER TABLE tenants ADD COLUMN sharing boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE tenants DROP COLUMN sharing;
ALTER TABLE messages DROP COLUMN via;
