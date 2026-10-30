// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// runDev handles developer helpers. They shell out to docker and go, and are meant to be run
// from the repository root.
func runDev(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return errors.New("dev: no subcommand given")
	}
	switch args[0] {
	case "up":
		if _, err := os.Stat(".env"); err != nil {
			return errors.New("dev up: no .env file; copy .env.example to .env and set the passwords")
		}
		return runCmd(out, "docker", "compose", "up", "-d", "--wait")
	case "down":
		return runCmd(out, "docker", "compose", "down")
	case "test":
		return runCmd(out, "go", "test", "./...")
	case "test-db":
		return runCmd(out, "go", "test", "-tags=db", "-count=1", "./...")
	default:
		usage(out)
		return fmt.Errorf("dev: unknown subcommand %q", args[0])
	}
}

func runCmd(out io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
