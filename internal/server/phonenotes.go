// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/notify"
	"seyal/internal/push"
)

// Phone notes (Phase 4, part 4a-2, D87): the "Phone notes" block on Me and the three small routes its
// script calls. Each person turns them on for their own browser only; nobody can turn them on for
// someone else. A browser's device push address and lock keys are saved for the login (auth.TurnOnDevice)
// and never shown on a page. Turning on is only offered when the server has phone notes switched on
// (SEYAL_PUSH=on with its keys): the browser itself then asks its push service for an address, which
// is already a call outside. This part sends no phone note yet.

// pushView is the Me block.
type pushView struct {
	Key     string      // the server's public push key, for the browser; "" means phone notes are off on this server
	Devices []deviceRow // the login's browsers with phone notes on
	// Here is "" when this business allows phone notes; "file" when its set-up file has them off
	// (notifications: off, the default), "paused" when an admin paused them (D89). A device serves every
	// business of the login, so Turn on still shows: other businesses may send to it.
	Here string
}

type deviceRow struct {
	ID, Label, Since, Opened string
}

// pushViewFor returns the Me block for the login. tx may be any transaction (auth.Devices
// switches to the login's scope and back).
func (s *Server) pushViewFor(r *http.Request, tx pgx.Tx, c *call) (pushView, error) {
	pv := pushView{Key: s.pushKey}
	ns, err := notify.LoadSettings(r.Context(), tx)
	if err != nil {
		return pv, err
	}
	switch {
	case !ns.On:
		pv.Here = "file"
	case ns.Paused():
		pv.Here = "paused"
	}
	devices, err := auth.Devices(r.Context(), tx, c.session.AccountID)
	if err != nil {
		return pv, err
	}
	now := time.Now().In(c.tenant.Zone)
	for _, d := range devices {
		pv.Devices = append(pv.Devices, deviceRow{ID: d.ID, Label: labelOr(d.Label),
			Since: shortDay(d.CreatedAt.In(c.tenant.Zone), now), Opened: shortDay(d.LastOpenedAt.In(c.tenant.Zone), now)})
	}
	return pv, nil
}

func labelOr(label string) string {
	if label == "" {
		return "A browser"
	}
	return label
}

// shortDay is "today", "3 Nov", or "3 Nov 2025" for another year.
func shortDay(t, now time.Time) string {
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return "today"
	case t.Year() == now.Year():
		return t.Format("2 Jan")
	}
	return t.Format("2 Jan 2006")
}

// subscription is what a browser's PushSubscription.toJSON() gives; only these parts are read. The
// page adds home when it runs as an iPhone Home Screen icon: that icon is this business's own app,
// with its own address, so only this business sends to it.
type subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Home bool `json:"home"`
}

// readSubscription reads a small JSON body from the page's script.
func readSubscription(w http.ResponseWriter, r *http.Request) (subscription, bool) {
	var sub subscription
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That wasn't a browser's phone-note details."})
		return sub, false
	}
	return sub, true
}

// turnOnsPerHour and hellosPerMinute are how often one login may turn phone notes on, and say hello.
// A person taps Turn on a few times at most, and a page says hello at most once a load; the limits
// stop a script from filling the devices table (each turn-on can add a row, and rows are kept).
const (
	turnOnsPerHour  = 20
	hellosPerMinute = 60
)

// tooMany answers a phone-note route called too often by one login.
func tooMany(w http.ResponseWriter) {
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "That was tried too many times. Please wait a while and try again."})
}

// loginKey is what the per-login limits count: the login, which may be used in several businesses.
func loginKey(c *call) string {
	if c.session.AccountID != "" {
		return c.session.AccountID
	}
	return c.tenant.ID + "/" + c.session.PersonID
}

// turnOnDevice saves this browser for phone notes (Me → Turn on for this phone).
func (s *Server) turnOnDevice(w http.ResponseWriter, r *http.Request, c *call) {
	if s.pushKey == "" {
		http.NotFound(w, r) // phone notes are off on this server
		return
	}
	if !s.turnOnRate.take(loginKey(c)) {
		tooMany(w)
		return
	}
	sub, ok := readSubscription(w, r)
	if !ok {
		return
	}
	origin, ok := s.origin(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This address can't be used for phone notes."})
		return
	}
	home := ""
	if sub.Home {
		home = c.tenant.ID
	}
	var id string
	err := db.WithAccount(r.Context(), s.app, c.session.AccountID, func(tx pgx.Tx) error {
		var err error
		id, err = auth.TurnOnDevice(r.Context(), tx, c.token,
			push.Device{Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth}, origin, deviceLabel(r.UserAgent()), home)
		return err
	})
	switch {
	case errors.Is(err, auth.ErrBadDevice):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "This browser's phone-note details can't be used. Please try another browser."})
		return
	case errors.Is(err, auth.ErrNoSession):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Please log in again."})
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"device": id})
}

// helloDevice is the page telling Seyal it was opened in this browser, with the browser's device push
// address (never shown back). The answer says whether phone notes are on here.
func (s *Server) helloDevice(w http.ResponseWriter, r *http.Request, c *call) {
	if !s.helloRate.take(loginKey(c)) {
		tooMany(w)
		return
	}
	sub, ok := readSubscription(w, r)
	if !ok {
		return
	}
	var h auth.Hello
	err := db.WithAccount(r.Context(), s.app, c.session.AccountID, func(tx pgx.Tx) error {
		var err error
		h, err = auth.HelloDevice(r.Context(), tx, c.token, sub.Endpoint)
		return err
	})
	if errors.Is(err, auth.ErrNoSession) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Please log in again."})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": h.State, "device": h.DeviceID, "label": h.Label})
}

// turnOffDevice ends one of the login's devices: Turn off on Me ("turned_off"), or the page found the
// browser's permission taken back ("blocked"). Then back to Me.
func (s *Server) turnOffDevice(w http.ResponseWriter, r *http.Request, c *call) {
	why := "turned_off"
	if r.PostFormValue("why") == "blocked" {
		why = "blocked"
	}
	err := db.WithAccount(r.Context(), s.app, c.session.AccountID, func(tx pgx.Tx) error {
		return auth.TurnOffDevice(r.Context(), tx, c.session.AccountID, r.PathValue("id"), why)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/me", http.StatusSeeOther)
}

// origin is the address the browser used for this request (https://host, or http:// in development),
// kept with its device for the links in its phone notes.
var plainHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

func (s *Server) origin(r *http.Request) (string, bool) {
	if !plainHost.MatchString(r.Host) {
		return "", false
	}
	scheme := "http"
	if s.secure {
		scheme = "https"
	}
	return scheme + "://" + strings.ToLower(r.Host), true
}

// deviceLabel names a browser in plain words from its description, e.g. "Chrome on Android", so a
// person can tell their devices apart on Me.
func deviceLabel(ua string) string {
	browser := "A browser"
	switch {
	case strings.Contains(ua, "Edg/") || strings.Contains(ua, "EdgA/") || strings.Contains(ua, "EdgiOS/"):
		browser = "Edge"
	case strings.Contains(ua, "Firefox/") || strings.Contains(ua, "FxiOS/"):
		browser = "Firefox"
	case strings.Contains(ua, "SamsungBrowser/"):
		browser = "Samsung Internet"
	case strings.Contains(ua, "Chrome/") || strings.Contains(ua, "CriOS/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	switch {
	case strings.Contains(ua, "Android"):
		return browser + " on Android"
	case strings.Contains(ua, "iPhone"):
		return browser + " on iPhone"
	case strings.Contains(ua, "iPad"):
		return browser + " on iPad"
	case strings.Contains(ua, "Windows"):
		return browser + " on Windows"
	case strings.Contains(ua, "CrOS"):
		return browser + " on a Chromebook"
	case strings.Contains(ua, "Macintosh"):
		return browser + " on a Mac"
	case strings.Contains(ua, "Linux"):
		return browser + " on Linux"
	}
	return browser
}

// writeJSON answers the page's script.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// The safe "go back to" address (next): a page opened without a login (for example from a phone
// note) comes back after the login and after choosing a new password. Only a path inside the same
// business is kept, written in plain letters, digits, "/", "-", "_" and ".", with no "//" and no "..":
// so the login page can never send anyone to another site, or to another business's address.
var nextPath = regexp.MustCompile(`^/t/[a-z0-9][a-z0-9-]{1,62}/[A-Za-z0-9/_.-]*$`)

func safeNext(slug, next string) bool {
	return slug != "" && len(next) <= 200 && nextPath.MatchString(next) && strings.HasPrefix(next, "/t/"+slug+"/") &&
		!strings.Contains(next, "//") && !strings.Contains(next, "..") && next != "/t/"+slug+"/"
}
