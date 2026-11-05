// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/internal/playbook"
	"seyal/internal/setup"
)

// runImport creates or updates a business from a set-up file.
func runImport(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: seyal import <set-up file>")
	}
	_, err := importFile(args[0], out)
	return err
}

// runSeed loads the fictional demo business and gives every demo person's login a fresh password,
// making the logins if needed and linking them to the demo people.
func runSeed(out io.Writer) error {
	f, err := importFile(filepath.Join("examples", "seed", "demo.yaml"), out)
	if err != nil {
		return err
	}
	ctx := context.Background()
	owner, app, err := pools(ctx)
	if err != nil {
		return err
	}
	defer owner.Close()
	defer app.Close()
	tenantID, err := tenantBySlug(ctx, app, f.Business.Slug)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nDemo logins (new passwords, shown only now):\n")
	for _, p := range f.People {
		password, err := setup.NewPassword(ctx, app, p.Email, tenantID)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  %-22s %-32s %s\n", p.Name, p.Email, password)
	}
	return nil
}

// runSetPassword gives a login a new random password and shows it once, making the login if there
// is none. The operator is trusted: this also resets a login used in several businesses. With
// --business, the login is also linked to that business's person with this email.
func runSetPassword(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("set-password", flag.ContinueOnError)
	flags.SetOutput(out)
	email := flags.String("email", "", "the login's email")
	slug := flags.String("business", "", "also link the login to this business's person with the email, e.g. demo")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" || flags.NArg() > 0 {
		return errors.New("usage: seyal set-password --email <email> [--business <slug>]")
	}
	ctx := context.Background()
	owner, app, err := pools(ctx)
	if err != nil {
		return err
	}
	defer owner.Close()
	defer app.Close()
	tenantID := ""
	if *slug != "" {
		if tenantID, err = tenantBySlug(ctx, app, *slug); err != nil {
			return err
		}
	}
	password, err := setup.NewPassword(ctx, app, *email, tenantID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "New password for %s (shown only now): %s\n", *email, password)
	fmt.Fprintln(out, "It is the login's one password, for every business it belongs to.")
	return nil
}

func importFile(path string, out io.Writer) (*setup.File, error) {
	playbooksDir, mastersDir := libraryDirs()
	lib, err := playbook.LoadLibrary(playbooksDir, mastersDir)
	if err != nil {
		return nil, err
	}
	f, err := setup.Load(path, lib)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	owner, app, err := pools(ctx)
	if err != nil {
		return nil, err
	}
	defer owner.Close()
	defer app.Close()
	sum, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		return nil, err
	}
	what := "updated"
	if sum.Created {
		what = "created"
	}
	fmt.Fprintf(out, "%s %q (%s): %d changes\n", what, f.Business.Name, f.Business.Slug, sum.Changes)
	return f, nil
}

// pools opens the owner and app connections from the settings.
func pools(ctx context.Context) (owner, app *pgxpool.Pool, err error) {
	ownerURL, err := config.Need("SEYAL_OWNER_DATABASE_URL")
	if err != nil {
		return nil, nil, err
	}
	appURL, err := config.Need("SEYAL_DATABASE_URL")
	if err != nil {
		return nil, nil, err
	}
	if owner, err = db.Open(ctx, ownerURL); err != nil {
		return nil, nil, err
	}
	if app, err = db.Open(ctx, appURL); err != nil {
		owner.Close()
		return nil, nil, err
	}
	return owner, app, nil
}

func tenantBySlug(ctx context.Context, app *pgxpool.Pool, slug string) (string, error) {
	var id string
	if err := app.QueryRow(ctx, "SELECT id::text FROM tenants WHERE slug = $1", slug).Scan(&id); err != nil {
		return "", fmt.Errorf("no business with the slug %q", slug)
	}
	return id, nil
}
