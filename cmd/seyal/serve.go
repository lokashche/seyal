// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/chase"
	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/notify"
	"seyal/internal/playbook"
	"seyal/internal/push"
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

// runServe starts the web app, and the chaser next to it (D76). The playbook and master-data folders
// are loaded as templates; each business manages its own processes and lists in Settings.
func runServe(out io.Writer) error {
	// Ctrl+C, or a service manager / Docker stopping it: finish the requests in progress, then stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	every, err := chase.Every(os.Getenv("SEYAL_CHASE_EVERY"))
	if err != nil {
		return err
	}

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
	tenants, err := db.Tenants(ctx, app) // the same list the chaser goes through each round
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
	pushPublic, pusher, err := pushKey(os.Getenv)
	if err != nil {
		return err
	}
	chat, err := chatLinks(os.Getenv)
	if err != nil {
		return err
	}
	sendOn, sendHosts, err := sending(os.Getenv)
	if err != nil {
		return err
	}
	reader := newReader(app, out, pusher)
	srv, err := server.New(server.Config{
		App: app, Store: media.Disk{Dir: mediaDir}, Library: lib, AI: aiSetup,
		SecureCookies: secureCookies(os.Getenv("SEYAL_SECURE_COOKIES"), os.Getenv("SEYAL_ADDR")),
		BehindProxy:   os.Getenv("SEYAL_BEHIND_PROXY") == "true",
		PushKey:       pushPublic,
		Poke:          reader.Poke,
		ChatLinks:     chat,
		SendOn:        sendOn,
		SendHosts:     sendHosts,
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
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: server.RequestLimit, // voice notes can be large
		WriteTimeout: server.RequestLimit, IdleTimeout: 2 * time.Minute,
	}
	// The chaser runs next to the web server and stops with it: on Ctrl+C it starts nothing new, and
	// serve waits for it within the same 10 seconds it gives the requests in progress.
	var chasing sync.WaitGroup
	loops(ctx, &chasing, app, out, every, reader)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
		if !waitFor(shutdown, &chasing) {
			fmt.Fprintln(out, "The chaser or the phone-note reader didn't stop in time; the database undoes its unfinished change.")
		}
	}()

	fmt.Fprintf(out, "Seyal is running. Press Ctrl+C to stop.\n")
	fmt.Fprintf(out, "  Log in at http://%s/\n", addr)
	fmt.Fprintf(out, "  %s\n", chase.Says(every))
	if pushPublic == "" {
		fmt.Fprintln(out, "  Phone notes are off (SEYAL_PUSH).")
	} else {
		fmt.Fprintln(out, "  Phone notes are on (SEYAL_PUSH=on): each person turns them on for their phone, on Me.")
	}
	if !sendOn {
		fmt.Fprintln(out, "  Sending to other apps is off (SEYAL_SEND).")
	} else {
		fmt.Fprintf(out, "  Sending to other apps is on (SEYAL_SEND=on), to %d allowed hosts (SEYAL_SEND_HOSTS).\n", len(sendHosts))
	}
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		stop() // the chaser stops too, before the database is closed
		chasing.Wait()
		return err
	}
	<-stopped // ListenAndServe returns as soon as Shutdown starts: wait for the requests and the chaser
	fmt.Fprintln(out, "Stopped.")
	return nil
}

// newReader makes the phone-note reader (D89), which posts through pusher (D90). A nil pusher
// (SEYAL_PUSH off) gives a reader that only moves each business's bookmark on, so switching phone
// notes on later never tells anyone about old work, and whose Poke does nothing.
func newReader(app db.Beginner, out io.Writer, pusher *push.Sender) *notify.Reader {
	var p notify.Pusher // stays nil, not a nil *push.Sender, when push is off
	if pusher != nil {
		p = pusher
	}
	return notify.New(app, log.New(out, "", log.LstdFlags), p)
}

// loops starts what runs next to the web server until ctx ends, each counted in wg: the chaser,
// unless every is 0 (SEYAL_CHASE_EVERY=off), and always the phone-note reader. The reader is its own
// loop, not a chaser task, so it runs with the chaser off too. It returns what it started.
func loops(ctx context.Context, wg *sync.WaitGroup, app db.Beginner, out io.Writer, every time.Duration, reader *notify.Reader) []string {
	var started []string
	if every > 0 {
		c := chase.New(app, log.New(out, "", log.LstdFlags))
		wg.Go(func() { c.Run(ctx, every, chase.FirstWait) })
		started = append(started, "chaser")
	}
	wg.Go(func() { reader.Run(ctx, notify.Every) })
	return append(started, "phone-note reader")
}

// waitFor waits for wg until ctx ends, and reports whether wg finished in time.
func waitFor(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
