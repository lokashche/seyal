# Send approved work to a Google Sheet

This guide sets up a Google Sheet that Seyal fills in by itself. When a process says
`do: send` (for example, "when a job is approved, send its main details"), Seyal posts the details
to a small script inside your sheet, and the script adds a row.

- It takes about 20 minutes. It is free.
- You need a Google account. A firm account is better than a person's own: if the person leaves, the
  script stops (see "If something goes wrong").
- Seyal needs no Google password and no Google Cloud project. Google's own "Apps Script" runs the
  script, in your account.
- **Nothing leaves Seyal until the server's admin switches sending on** (step 6).

Words used here:
- **Apps Script**: small programs that live inside a Google Sheet (Extensions → Apps Script).
- **Web app**: an Apps Script that has its own web address, so Seyal can post to it.
- **Signing secret**: a long random password (`whsec_…`) that Seyal and the script share. Seyal
  "signs" each post with it, and the script refuses any post without a good signature.
- **Set-up file**: the business's private file that `seyal import` reads (people, processes, other
  apps). It never goes into the code repository.

The sample script is [sheet-script.gs](sheet-script.gs), next to this guide.

## 1. Make the sheet

1. Make a new Google Sheet. Give it a name, for example **Kaveri jobs (demo)**.
2. Rename the first tab to **`Jobs`** (exactly; the script looks for this name).
3. Type these headers in row 1, one per column, from A to I, in this order:

   | A | B | C | D | E | F | G | H | I |
   |---|---|---|---|---|---|---|---|---|
   | Number | Client | Nature of work | Fee | Currency | Advance | Timeline (days) | State | Send id |

   Leave the rest of row 1 empty. The script checks this row before every write. If a column is
   added, moved or renamed, it refuses to write ("the header row doesn't match"), so it never writes
   into the wrong column.

Why these columns: they follow the process's `do: send` line. The demo process sends
`client, nature_of_work, fee_agreed, advance, timeline_days`. Seyal's row is always: the item number,
then each field in that order, the currency once (right after the first money field), then the state,
and the send id last. If your process sends other fields, change the headers here **and** the
`HEADERS` line at the top of the script, so they match.

## 2. Paste the script

1. In the sheet: **Extensions → Apps Script**. A new tab opens with a file called `Code.gs`.
2. Delete what is in `Code.gs`. Paste all of [sheet-script.gs](sheet-script.gs).
3. Give the project a name at the top left, for example **Seyal sheet script**. Press the save icon.
4. **Project Settings** (the gear on the left) → **Script properties** → **Add script property**:
   - Property: `SEYAL_SECRET`
   - Value: the signing secret that `seyal import` prints (step 5). You don't have it yet: come back
     to this step after step 5.

## 3. Deploy it as a web app

1. **Deploy → New deployment**. Click the gear next to "Select type" and choose **Web app**.
2. Description: `Seyal`. **Execute as: Me**. **Who has access: Anyone**.
3. Press **Deploy**.

"Anyone" is needed because Seyal has no Google login. **Anyone who has the address can call the
script, so keep the address secret, like a password.** The script itself refuses every call that
doesn't carry Seyal's signature, and it has no way to read the sheet (no `doGet`).

## 4. Allow access

The first deploy asks for permission.

1. Press **Authorize access** and choose your Google account.
2. For a personal account Google usually shows **"Google hasn't verified this app"**. Click
   **Advanced**, then **Go to Seyal sheet script (unsafe)**. This is your own script, so that is
   expected.
3. Press **Allow**. Because the script starts with `@OnlyCurrentDoc`, it asks only for **this one
   sheet**, not your whole Drive.

At the end, Google shows the **Web app URL**. It ends in `/exec`. Copy it.

## 5. Tell Seyal the address

In the business's **private set-up file** (on the server, in `deploy/private/`; never in the code
repository), add the sheet under `other_apps:`:

```yaml
other_apps:
  - name: jobs-sheet                    # the name a process uses: do: send, to: jobs-sheet
    label: Jobs sheet                   # what people see
    service: Google                     # shown as "Leaves Seyal: these values, to Google"
    url: https://script.google.com/macros/s/PASTE-YOUR-ADDRESS-HERE/exec
    redirect_to: [script.googleusercontent.com]   # Google answers with one jump to this host
    fields: [client, nature_of_work, fee_agreed, advance, timeline_days]   # the most it may receive
```

You don't need `expect:` for a `script.google.com` address: Seyal then counts a send as done only when
the script answers `"ok": true` (an Apps Script web app answers "200 OK" even when it fails).

Then import the file:

```
docker compose run --rm seyal import private/<business>.yaml
```

The first time it sees `jobs-sheet`, **it prints the signing secret once** (`whsec_…`). Copy it
into the script property `SEYAL_SECRET` (step 2.4). Seyal never shows it again, in the app or
anywhere else. If you missed it, or it leaked, make a new one with
`seyal import private/<business>.yaml --new-secret jobs-sheet` and put the new one in the script.

The process must also send to it. In the demo, Settings → Processes → Job evaluation → "Edit the
definition (advanced)": paste [../seed/job-evaluation-send.yaml](../seed/job-evaluation-send.yaml)
and save. Its `on: approved` part has the `do: send` line.

## 6. Switch sending on (the server's admin)

Only after the business has agreed to the values that leave Seyal for Google (the full list is under
"Limits" below). On the server, in `deploy/.env`, switch sending on and **add** the two Google hosts
to `SEYAL_SEND_HOSTS`. It is one comma-separated list: keep any hosts already there, or sends to
those other apps stop. For example:

```
SEYAL_SEND=on
SEYAL_SEND_HOSTS=<hosts already there>,script.google.com,script.googleusercontent.com
```

(With no other app yet, it is just `SEYAL_SEND_HOSTS=script.google.com,script.googleusercontent.com`.)
Then `docker compose up -d` (about a minute). Both hosts are needed: Google's answer jumps from the
first to the second.

## 7. Send a test

1. In Seyal, as an admin: **Settings → Processes**, the **Leaves Seyal** part at the bottom.
2. On the "Jobs sheet" line, press **[Send a test]**.
3. Seyal shows the exact data it sent and the answer. A good answer is "answered OK".
4. In the sheet, a new tab **Seyal test** appears with a row like
   `TEST-0000 · Acme Traders · 0 · INR · test · snd_…`. The `Jobs` tab stays clean: the script puts
   every test send on the test tab. It knows a test by the signed `"test": true` in the post, never
   by the number, so a process that numbers its items `TEST-…` still fills `Jobs`. You may delete the test tab at any time.

Then approve a real item. A few seconds later its row appears in `Jobs`, and the item's thread says
"Seyal: sent J-2026-0007 to “Jobs sheet” (it answered OK)".

## What the script does with each post

1. It checks the signature, which Seyal puts **inside** the post (Apps Script can't read a post's
   headers). It refuses a post that isn't signed with `SEYAL_SECRET`, a post signed more than
   5 minutes ago, and a post with anything added after the signature.
2. It reads the data only from the part that was signed.
3. It takes a lock, so two posts at the same moment can't add two rows. It saves its writes before
   it lets the lock go.
4. It looks for the item's number in column A (as text). If it is there, it **updates** that row; if
   not, it adds one. So when Seyal tries again after a lost answer, the row is not doubled. The
   number is always written as text, so a number like `0001` or `2026-01` is not turned into `1` or a
   date, and is found again.
5. If the row already holds a **newer** send of the same item (a higher count at the end of its send
   id, `snd_…_42` after `snd_…_41`), it leaves the row alone and answers OK. So a late retry of an
   older send never puts old values back.
6. Text that a sheet would treat as a formula (it starts with `=`, `+`, `-`, `@`, a tab or a
   carriage return) gets a `'` in front, so a client name like `=HYPERLINK(…)` stays plain text.
   Seyal does this too; the script checks again. Amounts stay numbers.
7. It always answers in JSON: `{"ok":true,"row":12}`, or `{"ok":false,"why":"no tab called Jobs"}`.

What Seyal does with the answer:

| The script answers | Seyal |
|---|---|
| `"ok": true` | done: "answered OK" |
| `"ok": false` and a reason (no `Jobs` tab, the header row doesn't match, a bad signature, `SEYAL_SECRET` missing) | stops, and tells the admins in the thread and on My Work. Fix it, then press **[Send again]** on the item. |
| no `"ok"` (the sheet was busy, Google had a problem, or the script stopped) | "answered without an OK": tries again after 5 minutes, 30 minutes, 2 hours and 6 hours, then gives up |
| a jump to Google's sign-in page | "needs sign-in": the script's sharing or deployment changed (step 3) |

## If something goes wrong

- **"script failed: no tab called Jobs"**: someone renamed the tab. Rename it back to `Jobs`, then
  **[Send again]** on the item.
- **"script failed: the header row doesn't match"**: row 1 of `Jobs` changed. Put it back as in
  step 1 (or change `HEADERS` in the script to match, and the process too).
- **"script failed: the signature does not match"**: `SEYAL_SECRET` in the script isn't the secret
  Seyal has. Copy it again, or make a new one (`--new-secret`, step 5).
- **"script failed: SEYAL_SECRET is not a valid whsec_ secret"**: the value in the script property
  was cut short or pasted wrongly. Copy the whole `whsec_…` secret again (step 2.4).
- **"answered without an OK"**, or later **"gave up sending … after N tries"**: the script did not
  answer `ok` true or false. A busy sheet clears by itself. If it keeps happening: in Apps Script,
  open **Executions** (on the left) and read the error. After you edit the script, check that you
  allowed access again if Google asked (step 4), and that you deployed it as a **New version** (see
  "You changed the script" below).
  Then press **[Send again]** on the item.
- **"script failed: the post is more than 5 minutes old"**: the server's clock is wrong. Fix the clock.
- **"needs sign-in"**: the deployment's "Who has access" is not "Anyone" any more, or the account
  that deployed it was removed. Deploy again (step 3).
- **You changed the script:** use **Deploy → Manage deployments → Edit (the pencil) → Version: New
  version → Deploy**. That keeps the same address. "New deployment" makes a *new* address, and the
  old one keeps running the old script.
- **The address leaked:** make a new deployment (a new address), put it in the set-up file, import
  again, then archive the old deployment (Manage deployments → Archive).
- **The person who deployed it leaves the firm:** the script stops. Deploy it again from a firm
  account, put the new address in the set-up file, and import again.
- **Stop it now:** an admin presses **[Pause]** on the line in Leaves Seyal. Sends wait (and after
  24 hours they expire, so nothing old floods the sheet when you press Resume).

## Limits

- Google allows 30 runs of a script at the same time per user, and 6 minutes per run. Seyal sends far
  less than that (at most 30 sends an hour to one app).
- What leaves Seyal for Google with each send:
  - the fields in the process's `do: send` line (only those also on the app's `fields:` list);
  - the item number, the process, the state, and the send time;
  - the business's short name (for example `acme-demo`), also inside the send id
    (`snd_acme-demo_42`), whose count shows how many sends the business has made;
  - only if the app has `link: true`: the item's link (the server's address and the item's id).

  Never the thread, voice notes, files, hand-over notes or people's emails.
- **Who can read it:** Google, and everyone the sheet is shared with. Anyone who can **edit** the
  sheet can also open its script (Extensions → Apps Script), read `SEYAL_SECRET` and change the
  script. Share edit access only with people you trust.
- The script keys rows by item number only. If one process sends the same item twice (say, at two
  different states), the row shows the newest send (the one with the highest count in its send id).

## Not tried yet

This sample was checked against Seyal's own signing code with stand-ins for Google's services, but
**not yet in a real Google Sheet**. Two things need the real try: that Google's answer is one jump to
`script.googleusercontent.com` (from community pages, not Google's own docs), and that text with
letters like `₹` is read back byte for byte, so its signature matches. If the test send works but a
real send says "the signature does not match", tell the Seyal maintainers.
