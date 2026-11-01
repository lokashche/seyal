# Seyal

**No forms. Just tell Seyal.**

Seyal is a chat-first operations system for small businesses. Staff report work and give
instructions by text or voice. Seyal turns each message into a work item, applies the
business's playbook (fields, approvals, deadlines), chases people until the work is done, and
keeps a tamper-evident ledger of every step.

> **Status:** early development (v0). Being tried in one firm; expect changes.

## Try it on your computer

You need [Go](https://go.dev/dl/) (the version in `go.mod`) and Docker. From a fresh clone:

```sh
cp .env.example .env              # then replace the "change-me" passwords in .env
go run ./cmd/seyal dev up         # start the local database (Docker)
go run ./cmd/seyal migrate        # create the tables
go run ./cmd/seyal seed           # load the fictional demo business; prints its logins
go run ./cmd/seyal serve          # open the address it prints, e.g. http://127.0.0.1:8080/t/demo/
```

Log in with one of the printed demo logins and tell Seyal about some work. Everything in the demo is
made up. To start again from nothing: `go run ./cmd/seyal dev reset` (local database only; it asks
first).

For the short `seyal ...` command, run `go install ./cmd/seyal` (it goes into Go's bin folder, which
must be on your PATH), and again after pulling or changing code.

## Develop

```sh
go run ./cmd/seyal dev test       # quick tests
go run ./cmd/seyal dev test-db    # database tests (needs "dev up")
go run ./cmd/seyal check          # check the sample processes and lists
```

Processes and lists are data (YAML), not code: see `examples/`. Each business keeps its own in its
database and changes them in Settings.

## Run it for a business

See [deploy/README.md](deploy/README.md): a small Linux server, Docker Compose (PostgreSQL, Seyal,
and Caddy for HTTPS), and encrypted nightly backups. Releases provide the same program as a
download for Linux, Windows and macOS, and as a container image.

## License

Copyright (C) 2026 The Seyal contributors.

Seyal is free software: you can redistribute it and/or modify it under the terms of the GNU
Affero General Public License as published by the Free Software Foundation, either version 3 of
the License, or (at your option) any later version. See [LICENSE](LICENSE).

Contributions are welcome under the terms in [CONTRIBUTING.md](CONTRIBUTING.md). To report a
security problem, see [SECURITY.md](SECURITY.md).
