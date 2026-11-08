// SPDX-License-Identifier: AGPL-3.0-or-later

// Command seyal is the Seyal server and its helper commands.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"seyal/internal/config"
)

// appName is the command name shown to users. Rename here (and the cmd/ folder) if needed.
const appName = "seyal"

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.0.1-dev"

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
		os.Exit(1)
	}
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return errors.New("no command given")
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(out, appName, version)
		return nil
	case "dev":
		return runDev(args[1:], out)
	case "migrate":
		return runMigrate(out)
	case "verify":
		return runVerify(args[1:], out)
	case "check":
		return runCheck(out)
	case "serve":
		return runServe(out)
	case "import":
		return runImport(args[1:], out)
	case "seed":
		return runSeed(out)
	case "set-password":
		return runSetPassword(args[1:], out)
	case "link":
		return runLink(args[1:], out)
	case "push":
		return runPush(args[1:], out)
	case "help", "-h", "--help":
		usage(out)
		return nil
	default:
		usage(out)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(out io.Writer) {
	fmt.Fprintf(out, `Usage: %[1]s <command>

Commands:
  version        print the version
  serve          start the web app (SEYAL_ADDR, default 127.0.0.1:8080) and the chaser
                 (SEYAL_CHASE_EVERY, default 5m; off to turn it off)
  dev up         start the local database (Docker)
  dev down       stop the local database
  dev test       run the quick tests
  dev test-db    run the database tests (needs "dev up")
  dev reset      wipe the LOCAL database and files, then load the demo (asks first)
  dev serve      run the web app and restart it when the code changes (--lan: phones on Wi-Fi too)
  dev chase      chase the LOCAL demo business once, as if on another day (--as-if 2026-11-09)
  migrate        apply all pending database migrations
  verify         check every ledger for tampering (--full: from the start)
  check          verify the integrity of the playbooks and master data
  import FILE    create or update a business from a set-up file
                 (--new-secret NAME: a new signing secret for that other app, shown once)
  seed           load the fictional demo business and print its logins
  set-password   give a login a new password (--email EMAIL [--business SLUG])
  link           add an existing login to a business (--email EMAIL --business SLUG)
  push keys      make the server's phone-note keys and print them as two .env lines
                 (nothing leaves this computer; keep the private key secret and backed up)
  help           show this help
`, appName)
}
