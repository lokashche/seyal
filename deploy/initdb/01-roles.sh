#!/bin/sh
# Runs once, when the database volume is first created.
# seyal_owner owns the schema and runs migrations.
# seyal_app is what the server uses: not an owner and never allowed to bypass row-level security.
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
	-v owner_pw="$SEYAL_OWNER_PASSWORD" -v app_pw="$SEYAL_APP_PASSWORD" <<'EOSQL'
CREATE ROLE seyal_owner LOGIN PASSWORD :'owner_pw' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE seyal_app   LOGIN PASSWORD :'app_pw'   NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;

CREATE DATABASE seyal      OWNER seyal_owner;
CREATE DATABASE seyal_test OWNER seyal_owner;

REVOKE ALL ON DATABASE seyal, seyal_test FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE seyal, seyal_test TO seyal_app;
EOSQL
