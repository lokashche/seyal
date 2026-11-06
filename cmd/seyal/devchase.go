// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	"seyal/internal/chase"
	"seyal/internal/config"
	"seyal/internal/db"
)

// devChase runs one round of the chaser for the demo business only, as if today were another day, so
// reminders can be tried without waiting days (D75, D76). Only "today" is pretend, and only for working
// out what is due: every stored time is real, and the events say "as_if". Like dev reset, it refuses a
// database that isn't on this computer.
func devChase(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("dev chase", flag.ContinueOnError)
	flags.SetOutput(out)
	asIf := flags.String("as-if", "", "the pretend today, e.g. 2026-11-09 (not before today)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *asIf == "" || flags.NArg() > 0 {
		return errors.New("usage: seyal dev chase --as-if <day, e.g. 2026-11-09>")
	}
	url, err := config.Need("SEYAL_DATABASE_URL")
	if err != nil {
		return err
	}
	if err := localOnly(url); err != nil {
		return fmt.Errorf("dev chase: %w", err)
	}
	ctx := context.Background()
	app, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	defer app.Close()

	n, err := chase.New(app, log.New(out, "", 0)).AsIf(ctx, chase.DemoSlug, *asIf)
	if err != nil {
		return fmt.Errorf("dev chase: %w", err)
	}
	day := *asIf
	if d, err := time.Parse(time.DateOnly, *asIf); err == nil {
		day = d.Format("Mon 2 Jan 2006")
	}
	switch n {
	case 0:
		fmt.Fprintf(out, "Chased the demo business as if it were %s: nothing was due.\n", day)
	case 1:
		fmt.Fprintf(out, "Chased the demo business as if it were %s: 1 thing done (see the lines above).\n", day)
	default:
		fmt.Fprintf(out, "Chased the demo business as if it were %s: %d things done (see the lines above).\n", day, n)
	}
	return nil
}
