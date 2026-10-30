# Seyal

**No forms. Just tell Seyal.**

Seyal is a chat-first operations system for small businesses. Staff report work and give
instructions by text or voice. Seyal turns each message into a work item, applies the
business's playbook (fields, approvals, deadlines), chases people until the work is done, and
keeps a tamper-evident ledger of every step.

> **Status:** early development (pre-alpha). Not ready for real data.

## Quick start (development)

You need Go 1.25+ and Docker.

```sh
cp .env.example .env              # then edit the passwords in .env
go run ./cmd/seyal dev up         # start the local database
go run ./cmd/seyal dev test       # run the tests
go run ./cmd/seyal version
```

To use the short `seyal ...` command, install it with `go install ./cmd/seyal`. This puts it in
Go's bin folder, which must be on your PATH. Run `go install ./cmd/seyal` again after pulling or
changing code.

## License

Copyright (C) 2026 The Seyal contributors.

Seyal is free software: you can redistribute it and/or modify it under the terms of the GNU
Affero General Public License as published by the Free Software Foundation, either version 3 of
the License, or (at your option) any later version. See [LICENSE](LICENSE).

Contributions are welcome under the terms in [CONTRIBUTING.md](CONTRIBUTING.md). To report a
security problem, see [SECURITY.md](SECURITY.md).
