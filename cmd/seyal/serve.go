// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/server"
)

// secureCookies decides whether login cookies are sent only over HTTPS: as set ("true" or "false"),
// otherwise yes unless the app listens only on this computer (development). Forgetting the setting on
// a real server must not send logins over plain HTTP.
func secureCookies(setting, addr string) bool {
	switch setting {
	case "true":
		return true
	case "false":
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	local := addr == "" || (err == nil && (host == "127.0.0.1" || host == "localhost" || host == "::1"))
	return !local
}

// runServe starts the web app. The playbook and master-data folders are loaded as templates; each
// business manages its own processes and lists in Settings.
func runServe(out io.Writer) error {
	// Ctrl+C, or a service manager / Docker stopping it: finish the requests in progress, then stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	// Businesses from before lists were kept per business get the lists they use from the templates.
	for _, t := range tenants {
		var added []string
		err := db.WithTenant(ctx, app, t.ID, func(tx pgx.Tx) error {
			var err error
			added, err = playbook.AdoptKinds(ctx, tx, lib)
			return err
		})
		if err != nil {
			fmt.Fprintf(out, "Warning: %s: %v\n", t.Slug, err)
		}
		if len(added) > 0 {
			fmt.Fprintf(out, "%s: lists now kept per business: %s\n", t.Slug, strings.Join(added, ", "))
		}
	}

	mediaDir := os.Getenv("SEYAL_MEDIA_DIR")
	if mediaDir == "" {
		mediaDir = "data/media"
	}
	aiSetup, err := ai.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	srv, err := server.New(server.Config{
		App: app, Store: media.Disk{Dir: mediaDir}, Library: lib, AI: aiSetup,
		SecureCookies: secureCookies(os.Getenv("SEYAL_SECURE_COOKIES"), os.Getenv("SEYAL_ADDR")),
		BehindProxy:   os.Getenv("SEYAL_BEHIND_PROXY") == "true",
	})
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
