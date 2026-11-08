// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/internal/playbook"
	"seyal/internal/send"
	"seyal/internal/setup"
)

// runImport creates or updates a business from a set-up file. --new-secret <name> (before or after
// the file, and more than once) gives that other app a new signing secret (D95).
func runImport(args []string, out io.Writer) error {
	path, newSecrets, err := importArgs(args)
	if err != nil {
		return err
	}
	_, err = importFile(path, out, newSecrets...)
	return err
}

// importArgs reads "import <file> [--new-secret <name>]...", with the flag before or after the file.
func importArgs(args []string) (path string, newSecrets []string, err error) {
	usage := errors.New("usage: seyal import <set-up file> [--new-secret <other app>]")
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--new-secret" || a == "-new-secret":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", nil, usage
			}
			i++
			newSecrets = append(newSecrets, args[i])
		case strings.HasPrefix(a, "--new-secret="):
			newSecrets = append(newSecrets, strings.TrimPrefix(a, "--new-secret="))
		case strings.HasPrefix(a, "-") || path != "":
			return "", nil, usage
		default:
			path = a
		}
	}
	if path == "" {
		return "", nil, usage
	}
	return path, newSecrets, nil
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
	fmt.Fprintln(out, "Log in at the server's address (\"/\"): one login works in every business it belongs to.")
	return nil
}

// runLink adds an existing login to a business: the login with the email now reaches that business's
// person with the same email (e.g. the admin of a second business who already logs in to the first).
// The password stays as it is.
func runLink(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("link", flag.ContinueOnError)
	flags.SetOutput(out)
	email := flags.String("email", "", "the login's email")
	slug := flags.String("business", "", "the business to add the login to, e.g. demo")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" || *slug == "" || flags.NArg() > 0 {
		return errors.New("usage: seyal link --email <email> --business <slug>")
	}
	ctx := context.Background()
	owner, app, err := pools(ctx)
	if err != nil {
		return err
	}
	defer owner.Close()
	defer app.Close()
	tenantID, err := tenantBySlug(ctx, app, *slug)
	if err != nil {
		return err
	}
	linked, err := setup.Link(ctx, app, *email, tenantID)
	if err != nil {
		return err
	}
	if !linked {
		fmt.Fprintf(out, "%s already reaches %s; nothing changed.\n", *email, *slug)
		return nil
	}
	fmt.Fprintf(out, "%s now reaches %s too, with the same password.\n", *email, *slug)
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

func importFile(path string, out io.Writer, newSecrets ...string) (*setup.File, error) {
	playbooksDir, mastersDir := libraryDirs()
	lib, err := playbook.LoadLibrary(playbooksDir, mastersDir)
	if err != nil {
		return nil, err
	}
	f, err := setup.Load(path, lib)
	if err != nil {
		return nil, err
	}
	if len(newSecrets) > 0 {
		f.NewSecrets = newSecrets
		if err := f.Check(lib); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
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
	otherAppsSaid(out, f, sum.Secrets, sum.RemovedApps, os.Getenv("SEYAL_SEND"), os.Getenv("SEYAL_SEND_HOSTS"))
	return f, nil
}

// otherAppsSaid prints, once, the signing secret of each other app that got one on this import, and
// says which other apps were removed (with their secrets) and which can't receive anything yet on
// this server (D95). Only here is a secret ever shown: copy it into the other app now.
func otherAppsSaid(out io.Writer, f *setup.File, secrets map[string]string, removed []string, sendSwitch, hostList string) {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		fmt.Fprintf(out, "Signing secret for other app %s (shown only now; copy it into that app):\n  %s\n", name, secrets[name])
	}
	for _, name := range removed {
		fmt.Fprintf(out, "Other app %s is no longer in the file, so it was removed and its signing secret thrown away. "+
			"If you list it again, it gets a new secret to copy into that app.\n", name)
	}
	if len(f.OtherApps) == 0 {
		return
	}
	if sendSwitch != "on" {
		fmt.Fprintln(out, "Sending to other apps is off on this server (SEYAL_SEND), so nothing goes to them yet.")
	}
	hosts, err := send.ParseHosts(hostList)
	if err != nil {
		fmt.Fprintf(out, "SEYAL_SEND_HOSTS: %v\n", err)
		return
	}
	for _, o := range f.OtherApps {
		off := send.Target{URL: o.URL, RedirectTo: o.RedirectTo}.Clean().HostsOff(hosts)
		if len(off) > 0 {
			fmt.Fprintf(out, "Other app %s: %s isn't in SEYAL_SEND_HOSTS, so nothing goes to it until you add it.\n",
				o.Name, strings.Join(off, ", "))
		}
	}
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
