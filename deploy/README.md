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

(Without Cloudflare: point an **A record** `seyal.<your-domain>` at the server's IP, set
`COMPOSE_PROFILES=caddy`, `SEYAL_DOMAIN` and `ACME_EMAIL` in `.env` in step 3, and open ports 80/443.)

## 3. Get Seyal and set it up

```sh
git clone https://github.com/<owner>/seyal.git ~/seyal     # or unpack a release download
cd ~/seyal/deploy
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
docker compose run --rm seyal set-password --business <slug> --email <your-email>
```

Open `https://seyal.<your-domain>/t/<slug>/`, log in, and add the others in Settings → People (each
gets a first password, shown once, and chooses their own at first login). Bring in lists from a
spreadsheet in Settings → Lists.

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
```

To go back, set the previous version and run the same two commands (a newer database migration may
need a restore; read the release notes).

## Good to know

- With the tunnel, nothing on the server is reachable from outside except SSH; with Caddy, only ports
  80 and 443. The database and Seyal never are.
- With the tunnel, Cloudflare decrypts and re-encrypts the traffic on its way (that is how it serves
  HTTPS for you). Cloudflare's free plan accepts uploads up to 100 MB per request.
- Logins are slowed down after 10 wrong passwords in 15 minutes, per address and per email.
- `docker compose logs -f seyal` shows what Seyal is doing; errors never include passwords or keys.
- AI is off until you set `SEYAL_AI_UNDERSTAND` in `.env`. Decide first what your provider may
  receive (see its data policy).
