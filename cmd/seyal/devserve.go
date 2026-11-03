// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// devServe runs the web app and restarts it whenever the code changes. Templates, styles and
// migrations are built into the program, so any change means: build again, apply new migrations,
// start the new server. A build that fails is shown and the running server is kept. With --lan,
// phones on the same Wi-Fi can open it (plain http, so the login cookie mustn't be "secure").
func devServe(args []string, out io.Writer) error {
	lan := slices.Contains(args, "--lan")
	env := os.Environ()
	if lan {
		port := "8080"
		if _, p, err := net.SplitHostPort(os.Getenv("SEYAL_ADDR")); err == nil && p != "" {
			port = p
		}
		env = append(env, "SEYAL_ADDR=0.0.0.0:"+port, "SEYAL_SECURE_COOKIES=false")
		for _, ip := range lanAddresses() {
			fmt.Fprintf(out, "dev serve: on a phone on the same Wi-Fi, open http://%s:%s/t/<business>/\n", ip, port)
		}
		fmt.Fprintln(out, "dev serve: (if Windows asks, allow it on Private networks only)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir, err := os.MkdirTemp("", "seyal-dev-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var running *exec.Cmd
	defer func() { halt(running) }()

	seen := snapshot()
	for n := 1; ; n++ {
		// Windows can't replace a program that is running, so each build gets its own file.
		bin := filepath.Join(dir, fmt.Sprintf("seyal-%d%s", n, exeSuffix()))
		started := time.Now()
		if err := runCmd(out, "go", "build", "-o", bin, "./cmd/seyal"); err != nil {
			fmt.Fprintln(out, "dev serve: the build failed (above); fix it and save, the old server keeps running")
		} else if err := devCmd(out, env, bin, "migrate").Run(); err != nil {
			fmt.Fprintln(out, "dev serve: migrating failed (above); fix it and save")
		} else {
			halt(running)
			running = devCmd(out, env, bin, "serve")
			if err := running.Start(); err != nil {
				return err
			}
			fmt.Fprintf(out, "dev serve: running (built in %.1fs); refresh the browser after a change\n", time.Since(started).Seconds())
		}
		// Wait for a change, or Ctrl+C.
		for {
			select {
			case <-ctx.Done():
				fmt.Fprintln(out, "\ndev serve: stopped")
				return nil
			case <-time.After(500 * time.Millisecond):
			}
			now := snapshot()
			if file := changed(seen, now); file != "" {
				time.Sleep(200 * time.Millisecond) // let an editor finish saving several files
				seen = snapshot()
				fmt.Fprintf(out, "dev serve: %s changed, rebuilding…\n", file)
				break
			}
		}
	}
}

// watched are the folders and kinds of file that make up the server. Tests don't, so they're left out.
var (
	watchedDirs = []string{"cmd", "internal", "web", "migrations", "examples"}
	watchedExts = []string{".go", ".html", ".css", ".js", ".sql", ".yaml", ".json"}
)

// snapshot is every watched file with the time it last changed.
func snapshot() map[string]time.Time {
	files := map[string]time.Time{}
	for _, root := range watchedDirs {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") || !slices.Contains(watchedExts, filepath.Ext(path)) {
				return nil
			}
			if info, err := d.Info(); err == nil {
				files[path] = info.ModTime()
			}
			return nil
		})
	}
	return files
}

// changed names one file that is new, changed or gone between two snapshots, or "" if none.
func changed(before, after map[string]time.Time) string {
	for path, t := range after {
		if old, ok := before[path]; !ok || !old.Equal(t) {
			return path
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			return path
		}
	}
	return ""
}

func devCmd(out io.Writer, env []string, bin string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, out, os.Stderr
	return cmd
}

// halt stops a running server and waits until its port is free again. Elsewhere it is asked to stop
// (it finishes requests in progress); Windows has no such signal, so it is ended at once.
func halt(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		cmd.Process.Kill()
	} else {
		cmd.Process.Signal(os.Interrupt)
		go func() { time.Sleep(5 * time.Second); cmd.Process.Kill() }()
	}
	cmd.Wait()
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// lanAddresses are this computer's addresses on the local network (IPv4, not loopback).
func lanAddresses() []string {
	var ips []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && n.IP.IsPrivate() {
			ips = append(ips, n.IP.String())
		}
	}
	return ips
}
