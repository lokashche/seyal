#!/bin/sh
# Seyal's nightly backup, run on the server by cron (see deploy/README.md):
#   1. checks the ledger (seyal verify) and stops if anything was tampered with;
#   2. dumps the database and packs the voice notes and files;
#   3. sends both, encrypted, to cloud storage with restic, keeping 30 daily and 12 monthly copies.
# restic encrypts on this server with RESTIC_PASSWORD before anything leaves it. Lose that password
# and the backups can't be read, so keep a copy somewhere safe (not only on this server).
#
# Settings: /etc/seyal/backup.env (chmod 600), with
#   RESTIC_REPOSITORY=s3:https://<endpoint>/<bucket>/seyal
#   RESTIC_PASSWORD=<long random password>
#   AWS_ACCESS_KEY_ID=<storage key id>
#   AWS_SECRET_ACCESS_KEY=<storage secret>
set -eu

cd "$(dirname "$0")"
. /etc/seyal/backup.env
export RESTIC_REPOSITORY RESTIC_PASSWORD AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

out=/var/backups/seyal
mkdir -p "$out"
chmod 700 "$out"
stamp=$(date -u +%Y-%m-%dT%H:%MZ)

echo "$stamp checking the ledger"
docker compose run --rm --no-deps seyal verify > "$out/verify.txt" 2>&1 || {
	echo "seyal verify FAILED; no backup taken. See $out/verify.txt" >&2
	exit 1
}

echo "$stamp dumping the database"
docker compose exec -T postgres pg_dump -U postgres -d seyal -Fc > "$out/seyal.dump.part"
mv "$out/seyal.dump.part" "$out/seyal.dump"

echo "$stamp packing voice notes and files"
docker run --rm -v seyal_media:/media:ro alpine:3.20 tar -C /media -cf - . > "$out/media.tar.part"
mv "$out/media.tar.part" "$out/media.tar"

echo "$stamp sending to $RESTIC_REPOSITORY"
restic backup --quiet --tag nightly "$out"
restic forget --quiet --prune --keep-daily 30 --keep-monthly 12
# Now and then, read back a part of the stored data to be sure it can be restored.
if [ "$(date +%d)" = "01" ]; then
	restic check --read-data-subset=5%
fi
echo "$stamp done"
