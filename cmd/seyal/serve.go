// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/server"
)

// runServe starts the web app. Before serving, it registers the current playbooks for every
// business; a playbook changed without a new version stops the start (see decision D13).
func runServe(out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	url, err := config.Need("SEYAL_DATABASE_URL")
	if err != nil {
		return err
	}
	app, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	defer app.Close()

	playbooksDir, mastersDir := libraryDirs()
	lib, err := playbook.LoadLibrary(playbooksDir, mastersDir)
	if err != nil {
		return err
	}
	rows, _ := app.Query(ctx, "SELECT id::text, slug FROM tenants ORDER BY slug")
	tenants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Slug string }])
	if err != nil {
		return err
	}
	for _, t := range tenants {
		err := db.WithTenant(ctx, app, t.ID, func(tx pgx.Tx) error {
			for _, p := range lib.Playbooks {
				if _, _, err := playbook.Register(ctx, tx, p); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("business %s: %w", t.Slug, err)
		}
	}

	mediaDir := os.Getenv("SEYAL_MEDIA_DIR")
	if mediaDir == "" {
		mediaDir = "data/media"
	}
	srv, err := server.New(app, media.Disk{Dir: mediaDir}, os.Getenv("SEYAL_SECURE_COOKIES") == "true")
	if err != nil {
		return err
	}
	addr := os.Getenv("SEYAL_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080" // this computer only, unless set otherwise
	}
	hs := &http.Server{
		Addr: addr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, // voice notes can be large
		WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()

	fmt.Fprintf(out, "Seyal is running. Press Ctrl+C to stop.\n")
	for _, t := range tenants {
		fmt.Fprintf(out, "  http://%s/t/%s/\n", addr, t.Slug)
	}
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	fmt.Fprintln(out, "Stopped.")
	return nil
}
