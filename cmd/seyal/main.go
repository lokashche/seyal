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
  serve          start the web app (SEYAL_ADDR, default 127.0.0.1:8080)
  dev up         start the local database (Docker)
  dev down       stop the local database
  dev test       run the quick tests
  dev test-db    run the database tests (needs "dev up")
  migrate        apply all pending database migrations
  verify         check every ledger for tampering (--full: from the start)
  check          verify the integrity of the playbooks and master data
  import FILE    create or update a business from a set-up file
  seed           load the fictional demo business and print its logins
  set-password   give a person a new password (--business SLUG --email EMAIL)
  help           show this help
`, appName)
}
