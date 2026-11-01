// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	case "reset":
		return devReset(os.Stdin, out)
	default:
		usage(out)
		return fmt.Errorf("dev: unknown subcommand %q", args[0])
	}
}

// devReset starts the local database again from nothing: it deletes the database container's data
// and the stored voice notes and files, then migrates and loads the demo business. It refuses unless
// the database is on this computer, and asks the person to type "reset" first.
func devReset(in io.Reader, out io.Writer) error {
	if err := localOnly(os.Getenv("SEYAL_OWNER_DATABASE_URL"), os.Getenv("SEYAL_DATABASE_URL")); err != nil {
		return err
	}
	media := os.Getenv("SEYAL_MEDIA_DIR")
	if media == "" {
		media = filepath.Join("data", "media")
	}
	fmt.Fprintf(out, "This deletes EVERYTHING in the local database and in %s, then loads the demo business.\n"+
		"Type reset to go on: ", media)
	if !confirmed(in, "reset") {
		fmt.Fprintln(out, "Nothing was changed.")
		return nil
	}
	if err := runCmd(out, "docker", "compose", "down", "--volumes"); err != nil {
		return err
	}
	if err := clearMedia(media); err != nil {
		return err
	}
	if err := runCmd(out, "docker", "compose", "up", "-d", "--wait"); err != nil {
		return err
	}
	if err := runMigrate(out); err != nil {
		return err
	}
	return runSeed(out)
}

// localOnly refuses database addresses that aren't on this computer, so a reset can never reach a
// real server.
func localOnly(urls ...string) error {
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if raw == "" || err != nil {
			return errors.New("dev reset: the database addresses aren't set; check .env")
		}
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return fmt.Errorf("dev reset: refused, the database is on %q, not this computer", h)
		}
	}
	return nil
}

// confirmed reads one line and reports whether it is exactly word.
func confirmed(in io.Reader, word string) bool {
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.TrimSpace(line) == word
}

// clearMedia deletes the stored files, but only from a folder inside this one.
func clearMedia(dir string) error {
	if filepath.IsAbs(dir) || !filepath.IsLocal(dir) {
		return fmt.Errorf("dev reset: refused to delete %s; only a folder inside this one is cleared", dir)
	}
	return os.RemoveAll(dir)
}

func runCmd(out io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
