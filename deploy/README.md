# Running Seyal for a business

One small Linux server runs everything with Docker Compose: PostgreSQL and Seyal, reached over HTTPS
through a **Cloudflare Tunnel** (the server opens no ports at all; Cloudflare serves the address and
its certificate) or, without Cloudflare, through **Caddy** (ports 80/443, its own certificate).
Backups go, encrypted, to cloud storage every night.

**You need:** a Linux server (Ubuntu 24.04 LTS works; 2 GB RAM and 25 GB disk are plenty for one
firm), a domain name on Cloudflare (for the tunnel), and an S3-compatible storage bucket for backups
(for example Backblaze B2, AWS S3 or Cloudflare R2).

Commands below are run on the server as a user who can use `sudo`. `<…>` means "put your value".

## 1. Prepare the server

```sh
sudo apt update && sudo apt upgrade -y
sudo apt install -y docker.io docker-compose-v2 restic ufw unattended-upgrades
sudo usermod -aG docker $USER            # log out and in again afterwards
sudo ufw allow OpenSSH && sudo ufw enable   # with the tunnel, nothing else needs to be open
# (with Caddy instead: also  sudo ufw allow 80 && sudo ufw allow 443)
```

Use SSH keys, not passwords, to log in to the server.

## 2. Create the tunnel (Cloudflare)

1. Cloudflare dashboard → **Zero Trust** → **Networks** → **Tunnels** → **Create a tunnel** →
   **Cloudflared**, name it `seyal`.
2. On the "install connector" page, choose **Docker** and copy only the **token** (the long text after
   `--token`). Don't run the command shown: Seyal's compose file runs the connector.
3. **Public hostname**: subdomain `seyal`, your domain, service type **HTTP**, URL **`seyal:8080`**.
   Cloudflare creates the DNS record by itself.

(**Already have a tunnel** running elsewhere on your network, e.g. for a Proxmox server? Don't create
another one. Leave `COMPOSE_PROFILES` empty in step 3, publish Seyal's port on the server's LAN
address with a `deploy/compose.override.yaml` (Compose reads it by itself; it isn't in the repository):

```yaml
services:
  seyal:
    ports:
      - "<server-lan-ip>:8080:8080"
```

and in your existing tunnel add a public hostname with service **HTTP**, URL `<server-lan-ip>:8080`.
Docker opens published ports past ufw, so other machines on the LAN can reach port 8080 too. Let only
the tunnel's machine in (`<lan-interface>` is the one holding `<server-lan-ip>` in `ip -br addr`):

```sh
sudo tee /etc/systemd/system/seyal-port-lock.service >/dev/null <<'EOF'
[Unit]
Description=Only the tunnel may reach Seyal's port 8080
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'iptables -C DOCKER-USER -i <lan-interface> -p tcp --dport 8080 ! -s <tunnel-ip> -j DROP 2>/dev/null || iptables -I DOCKER-USER -i <lan-interface> -p tcp --dport 8080 ! -s <tunnel-ip> -j DROP'

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload && sudo systemctl enable --now seyal-port-lock
```
)

(Without Cloudflare: point an **A record** `seyal.<your-domain>` at the server's IP, set
`COMPOSE_PROFILES=caddy`, `SEYAL_DOMAIN` and `ACME_EMAIL` in `.env` in step 3, and open ports 80/443.)

## 3. Get Seyal and set it up

```sh
git clone https://github.com/<owner>/seyal.git ~/seyal     # or unpack a release download
cd ~/seyal/deploy
mkdir -p private   # the business's set-up file goes here (if Docker makes it, it belongs to root)
cp .env.example .env && chmod 600 .env
nano .env        # CLOUDFLARE_TUNNEL_TOKEN, long random passwords (openssl rand -base64 24 | tr -d '/+='), AI keys
```

Choose the image in `.env`:
- a release: `SEYAL_IMAGE=ghcr.io/<owner>/seyal:v0.1.0`, then `docker compose pull`;
- or this copy of the code: keep `SEYAL_IMAGE=seyal:local`, then `docker compose build`.

```sh
docker compose up -d
docker compose ps                         # all running; "migrate" exited 0
docker compose logs cloudflared | grep -i registered   # the tunnel is connected
# (with Caddy: docker compose logs caddy | grep -i certificate)
```

## 4. Create the business

Write the business's private set-up file (same format as `examples/seed/demo.yaml`: name, currency,
time zone, financial year, roles, people, processes) in `~/seyal/deploy/private/` on the server only.
Never put it in the repository.

```sh
docker compose run --rm seyal import private/<business>.yaml
docker compose run --rm seyal set-password --email <your-email> --business <slug>
```

`set-password` makes your **login** (one email and password for the whole server), links it to your
person in the business (the set-up file must list you with that email), and shows the password once.
Open `https://seyal.<your-domain>/` and log in; with one business you go straight into it (its own
address is `/t/<slug>/`, which is what a phone installs). Change the password on the Me page.

Add the others in Settings → People: each gets a one-time link (24 hours) to send them. Whoever opens
it chooses a password, or, if they already use Seyal on this server, logs in with theirs to add the
business. Someone who forgot theirs: Edit → **Make a new link**. A business's link resets a password
only if the login is used in that business alone. When it is also used in another business, Settings
says so and makes no link; when it is in no business at all (e.g. an email edit unlinked it), the
link asks for its current password. Either way you reset it on the server, in
`~/seyal/deploy`, with `docker compose run --rm seyal set-password --email <their-email>`. It shows the
new password once, for you to pass on to them; they change it on the Me page. Bring in lists from a
spreadsheet in Settings → Lists.

### A second business

```sh
docker compose run --rm seyal import private/<second-business>.yaml
docker compose run --rm seyal link --email <your-email> --business <second-slug>
```

`link` adds your existing login to the second business (you must be in its set-up file with the same
email); your password stays the same. Log in at `https://seyal.<your-domain>/` as before: with two
businesses you choose one, and the **Me** page switches between them. Staff of the second business
get links from its Settings → People as above; someone already on the server joins with their own
password.

`link` adds whatever login has that email, so whoever knows that login's password gets in: use it
only for a login you know is that person's (e.g. they log in with it today).

## 5. Nightly backups

Create `/etc/seyal/backup.env` (`sudo mkdir -p /etc/seyal`, then `sudo nano /etc/seyal/backup.env`,
then `sudo chmod 600 /etc/seyal/backup.env`):

```sh
RESTIC_REPOSITORY=s3:https://<endpoint>/<bucket>/seyal
RESTIC_PASSWORD=<long random password>
AWS_ACCESS_KEY_ID=<storage key id>
AWS_SECRET_ACCESS_KEY=<storage secret>
```

**Write the RESTIC_PASSWORD down somewhere safe away from the server** (a password manager): without
it the backups can't be read.

```sh
sudo sh -c '. /etc/seyal/backup.env && export RESTIC_REPOSITORY RESTIC_PASSWORD AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY && restic init'
sudo ~/seyal/deploy/backup.sh             # first backup, by hand
sudo crontab -e                           # then add the nightly run (02:30):
# 30 2 * * * /home/<user>/seyal/deploy/backup.sh >> /var/log/seyal-backup.log 2>&1
```

Each run checks the ledger first (`seyal verify`) and refuses to back up a tampered one.

## 6. Restore (try it once, before you need it)

On the same or a new server with steps 1–3 done:

```sh
sudo sh -c '. /etc/seyal/backup.env && export RESTIC_REPOSITORY RESTIC_PASSWORD AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY && restic restore latest --target /tmp/restore'
cd ~/seyal/deploy
docker compose stop seyal
docker compose exec -T postgres pg_restore -U postgres -d seyal --clean --if-exists < /tmp/restore/var/backups/seyal/seyal.dump
docker run --rm -i -v seyal_media:/media alpine:3.20 tar -C /media -xf - < /tmp/restore/var/backups/seyal/media.tar
docker compose up -d
docker compose run --rm seyal verify --full
```

## Start again (test installs only)

While trying things out, you can wipe everything and start fresh. **This deletes the database and
all voice notes and files**; your `.env`, `private/` files, the tunnel and backups already taken stay.

```sh
cd ~/seyal/deploy
docker compose down --volumes      # stop and DELETE all data
docker compose up -d               # fresh, empty database (migrations run again)
docker compose run --rm seyal seed # optional: the fictional demo business, to play with
```

- Database passwords are fixed when the database is first created: change them in `.env` only
  together with a wipe, or Seyal can't connect.
- While testing, back up to a separate path (e.g. `.../seyal-backups/seyal-test`), so test
  snapshots never mix with real ones; switch to the real path at go-live.
- A good test: deploy, seed, take one backup, wipe, then do the restore drill above. That proves the
  backups really work. Then wipe once more and import your real set-up file.

## Updating

```sh
cd ~/seyal/deploy
nano .env                                 # SEYAL_IMAGE=ghcr.io/<owner>/seyal:<new version>
docker compose pull && docker compose up -d   # migrations run first, automatically
docker compose logs migrate               # what was applied, and any note a migration left for you
```

To go back, set the previous version and run the same two commands (a newer database migration may
need a restore; read the release notes).

### Upgrading to one login (the release with migrations 00015 and 00016)

This release moves passwords from each business into one login per email for the whole server. Try
it on a copy first, on your own computer, in the repository folder with `seyal dev up` running (Git
Bash):

1. Get the latest backup's `seyal.dump` out of restic as in step 6 (`restic restore latest --target
   /tmp/restore`; the file is `/tmp/restore/var/backups/seyal/seyal.dump`) and copy it into the
   repository folder on your computer (`*.dump` is git-ignored there; the commands below read it
   from there). Check out the release you are about to deploy (`git checkout <tag>`), so `migrate`
   runs its migrations.
2. Restore it into a new database owned by `seyal_owner` (one made the default way belongs to
   `postgres`, and the migrations then can't create anything in it):

   ```sh
   docker compose exec -T postgres psql -U postgres -c "CREATE DATABASE seyal_rehearse OWNER seyal_owner"
   docker compose exec -T postgres psql -U postgres -c "GRANT CONNECT, TEMPORARY ON DATABASE seyal_rehearse TO seyal_app"
   docker compose exec -T postgres pg_restore -U postgres -d seyal_rehearse < seyal.dump
   ```
3. Point both addresses at it and run the new version's migrations. The passwords are the ones in
   your `.env`; `<port>` is the port Postgres listens on on your computer (`SEYAL_DB_PORT` in your
   `.env`, 5432 if it isn't set). **Don't skip the check line:** without the two `export` lines (or
   with a typo in them), Seyal falls back to the addresses in your `.env` and migrates your everyday
   database instead of the copy.

   ```sh
   export SEYAL_OWNER_DATABASE_URL='postgres://seyal_owner:<owner password>@localhost:<port>/seyal_rehearse?sslmode=disable'
   export SEYAL_DATABASE_URL='postgres://seyal_app:<app password>@localhost:<port>/seyal_rehearse?sslmode=disable'
   # check: both lines must say .../seyal_rehearse?sslmode=disable (no passwords are printed)
   echo "owner: ${SEYAL_OWNER_DATABASE_URL##*@}"; echo "app:   ${SEYAL_DATABASE_URL##*@}"
   go run ./cmd/seyal migrate
   ```

   If either check line is empty or names another database, stop: run the `export` lines again in
   this same terminal.

   Read its notes (counts only, no names), e.g. `link_existing_logins: 2 login(s) left unlinked` and
   `accounts_only: 0 person(s) unlinked (changed since 00015), 2 person(s) left without a login`. The
   count is the people who are in more than one business, once for each newer business (see below).
4. Run `go run ./cmd/seyal verify --full`: every business's ledger must be OK.
5. See who they are, so you know whom to link (read-only; it prints their emails and roles on your
   screen). Those with `admin` among their roles get `seyal link` first (see below):

   ```sh
   docker compose exec -T postgres psql -U postgres -d seyal_rehearse -c "SELECT t.slug, p.login_email, coalesce(string_agg(pr.role_key, ', ' ORDER BY pr.role_key), '') AS roles FROM persons p JOIN tenants t ON t.id = p.tenant_id LEFT JOIN person_roles pr ON pr.tenant_id = p.tenant_id AND pr.person_id = p.id AND pr.revoked_at IS NULL WHERE p.status = 'active' AND p.account_id IS NULL AND p.password_hash IS NOT NULL GROUP BY 1, 2 ORDER BY 1, 2"
   ```
6. Clean up: `docker compose exec -T postgres psql -U postgres -c "DROP DATABASE seyal_rehearse"`,
   delete the copied `seyal.dump`, close the terminal (or `unset` the two addresses), and go back to
   your usual branch (`git checkout main`).
7. Then update the live server as above (a backup first is never wrong). Right after it, run step
   5's query again on the live database (on the server, in `~/seyal/deploy`, with `-d seyal` instead
   of `-d seyal_rehearse`): people may have joined or changed since the backup. Link the people it
   lists as below.

- **Staff stay logged in:** an old login cookie is swapped for the new one on their next visit, and
  passwords carry over. Nearly everyone keeps working without doing anything; the exceptions are next.
- **Someone in two or more businesses** keeps the password of the oldest business they are in (the
  one set up first) and, for now, can't log in to the newer ones, even with the same password there:
  each stored password is scrambled with its own random salt, so the upgrade can't tell two of them
  are the same. Their old login to a newer business stops working. For each newer business:
  - if they are an admin there (e.g. you, running two firms), or nobody there can make them a link,
    run on the server, in `~/seyal/deploy`:
    `docker compose run --rm seyal link --email <their-email> --business <newer-slug>`.
    They keep the older business's password. Do this first for admins: they can then help the rest.
  - otherwise its admin makes them a new link (Settings → People → Edit → **Make a new link**): they
    type the older business's password to add it.
- **Someone disabled at the upgrade** gets no login. If you enable them again later, their old
  password no longer works: make them a new link (Edit → **Make a new link**).
- **New:** log in at `https://seyal.<your-domain>/`. The old `/t/<slug>/` addresses (and installed
  phone apps) keep working.

## Good to know

- With the tunnel, nothing on the server is reachable from outside except SSH; with Caddy, only ports
  80 and 443. The database and Seyal never are.
- With the tunnel, Cloudflare decrypts and re-encrypts the traffic on its way (that is how it serves
  HTTPS for you). Cloudflare's free plan accepts uploads up to 100 MB per request.
- After 10 wrong passwords in 15 minutes (per address and per email), logging in is refused, even
  with the right password, until 15 minutes have passed since the first of them. Wrong passwords on a join link and wrong
  current passwords on the "change password" page count too.
- **Two known limits of one login per email** (accepted for now):
  - A join link's page tells whoever opens it that its email already has a Seyal login ("You
    already have a Seyal login (<email>)"). Send each link only to its person.
  - A business's admin can add an email that nobody on the server uses yet and open its link
    themselves, which makes the login for that email. When the email's real owner is later added
    elsewhere, their link asks for a password they never chose. Only you can sort it out: have that
    business's admin change the email of the person with that email (only a new email unlinks the
    login; disabling the person doesn't, and they could be enabled again), then give the login a new
    password with
    `docker compose run --rm seyal set-password --email <their-email>` and pass it to its real owner,
    who then uses their link.
- `docker compose logs -f seyal` shows what Seyal is doing; errors never include passwords or keys.
- AI is off until you set `SEYAL_AI_UNDERSTAND` in `.env`. Decide first what your provider may
  receive (see its data policy). Turning voice notes into words (`SEYAL_AI_TRANSCRIBE`) sends the
  recordings themselves to the provider: check its terms for audio (training use, how long it keeps
  them) before switching it on. The optional tidy-up (`SEYAL_AI_TIDY`) sends the words heard to a
  chat model too. The speech service is paid per minute: Seyal sends at most 240 minutes of audio a
  day per business and 60 per login (`SEYAL_AI_TRANSCRIBE_DAY_MINUTES`,
  `SEYAL_AI_TRANSCRIBE_LOGIN_DAY_MINUTES`).
