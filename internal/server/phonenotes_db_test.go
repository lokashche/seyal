//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"seyal/internal/push"
	"seyal/internal/server"
)

// withPush is the same site with phone notes switched on for the server (SEYAL_PUSH=on): Me then
// offers Turn on. Nothing is sent anywhere: this part has no sender, and the device push addresses
// below are made up.
func (s *site) withPush(t *testing.T) (*site, string) {
	t.Helper()
	keys, err := push.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{App: s.app, Store: s.store, Library: s.lib, AI: s.ai, PushKey: keys.Public()})
	if err != nil {
		t.Fatal(err)
	}
	p := *s
	p.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(p.srv.Close)
	return &p, keys.Public()
}

// browserSub is what a browser's PushSubscription.toJSON() gives, with a made-up address.
func browserSub(t *testing.T) map[string]any {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	return map[string]any{
		"endpoint":       "https://fcm.googleapis.com/fcm/send/test-" + rand.Text(),
		"expirationTime": nil,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(secret),
		},
	}
}

func (s *site) postJSON(c *http.Client, path string, body any, header map[string]string) (int, map[string]string) {
	s.t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]string{}
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// endedWhy is why a device ended ("" while on), read as the owner.
func (s *site) endedWhy(id string) string {
	s.t.Helper()
	var why string
	if err := s.owner.QueryRow(context.Background(), `SELECT coalesce(ended_why, '') FROM push_devices WHERE id = $1`, id).Scan(&why); err != nil {
		s.t.Fatal(err)
	}
	return why
}

// TestPhoneNotesOnMe: the Me block and its routes. With phone notes off on the server, Me says so and
// offers nothing to turn on, and the route is not there. With them on, a browser turns them on, says
// hello, shows up in the list (never with its device push address), and is turned off; another site
// can't post to the routes; logging out ends the device.
func TestPhoneNotesOnMe(t *testing.T) {
	off := newSite(t)
	me := "/t/" + off.slug + "/me"
	a := off.logIn(arjun)
	if _, page, _ := off.get(a, me); !strings.Contains(page, "aren't switched on for this Seyal server") ||
		strings.Contains(page, "Turn on for this phone") {
		t.Errorf("with phone notes off on the server, Me doesn't say so:\n%s", page)
	}
	if code, _ := off.postJSON(a, "/t/"+off.slug+"/push/devices", browserSub(t), nil); code != http.StatusNotFound {
		t.Errorf("turning on with phone notes off on the server = %d, want 404", code)
	}
	if _, page, _ := off.get(a, "/t/"+off.slug+"/password"); strings.Contains(page, "Phone notes stop") {
		t.Error("with phone notes off on the server, the password page talks about them")
	}

	s, key := off.withPush(t)
	base := "/t/" + s.slug
	a = s.logIn(arjun)
	_, page, _ := s.get(a, me)
	for _, want := range []string{`data-key="` + key + `"`, "Turn on for this phone", "title or a hand-over note",
		"tap Share, then Add to Home Screen", "needs iOS 16.4", "Android: Chrome menu", "Checking this phone"} {
		if !strings.Contains(page, want) {
			t.Errorf("Me doesn't show %q", want)
		}
	}

	sub := browserSub(t)
	endpoint := sub["endpoint"].(string)
	code, got := s.postJSON(a, base+"/push/devices", sub, nil)
	device := got["device"]
	if code != http.StatusOK || device == "" {
		t.Fatalf("turn on = %d %v", code, got)
	}
	if code, got := s.postJSON(a, base+"/push/hello", map[string]string{"endpoint": endpoint}, nil); code != http.StatusOK ||
		got["state"] != "on" || got["device"] != device || got["label"] != "Chrome on Android" {
		t.Errorf("hello = %d %v, want on, this device, Chrome on Android", code, got)
	}
	_, page, _ = s.get(a, me)
	if !strings.Contains(page, `data-device="`+device+`"`) || !strings.Contains(page, "Also on: Chrome on Android, since today, last opened today") {
		t.Errorf("Me doesn't list the device:\n%s", page)
	}
	if strings.Contains(page, endpoint) || strings.Contains(page, sub["keys"].(map[string]string)["p256dh"]) {
		t.Error("Me shows the device push address or a lock key")
	}
	// Each Turn off names its device, for a screen reader.
	if !strings.Contains(page, `aria-label="Turn off Chrome on Android, since today, last opened today"`) ||
		!strings.Contains(page, `aria-label="Turn off for this phone"`) {
		t.Errorf("Me's Turn off buttons don't name their device:\n%s", page)
	}
	// A new password stops phone notes here too, and the page says so before it is chosen.
	if _, page, _ := s.get(a, base+"/password"); !strings.Contains(page, "Phone notes stop on all your devices, this one too") {
		t.Errorf("the password page doesn't say phone notes stop:\n%s", page)
	}

	t.Run("bad details and other sites are refused", func(t *testing.T) {
		if code, _ := s.postJSON(a, base+"/push/devices", "not a subscription", nil); code != http.StatusBadRequest {
			t.Errorf("a body that isn't a subscription = %d, want 400", code)
		}
		bad := browserSub(t)
		bad["endpoint"] = "https://push.kaveri-demo.example/" + rand.Text()
		if code, got := s.postJSON(a, base+"/push/devices", bad, nil); code != http.StatusUnprocessableEntity ||
			strings.Contains(got["error"], "kaveri-demo") {
			t.Errorf("an address that isn't a push service = %d %v, want 422 without the address", code, got)
		}
		cross := map[string]string{"Sec-Fetch-Site": "cross-site"}
		for _, path := range []string{base + "/push/devices", base + "/push/hello"} {
			if code, _ := s.postJSON(a, path, browserSub(t), cross); code != http.StatusForbidden {
				t.Errorf("cross-site POST %s = %d, want 403", path, code)
			}
		}
		if code, _, _ := s.post(a, base+"/push/devices/"+device+"/off", nil, cross); code != http.StatusForbidden {
			t.Errorf("cross-site turn off = %d, want 403", code)
		}
		if code, _ := s.postJSON(s.browser(), base+"/push/devices", browserSub(t), nil); code != http.StatusSeeOther {
			t.Errorf("turning on without a login = %d, want the login page", code)
		}
	})

	t.Run("an iPhone Home Screen icon or a Mac Dock app belongs to this business", func(t *testing.T) {
		home := func(body map[string]any) string {
			t.Helper()
			code, got := s.postJSON(a, base+"/push/devices", body, nil)
			if code != http.StatusOK {
				t.Fatalf("turn on = %d %v", code, got)
			}
			var tenant string
			if err := s.owner.QueryRow(context.Background(), `SELECT coalesce(home_tenant::text, '') FROM push_devices WHERE id = $1`,
				got["device"]).Scan(&tenant); err != nil {
				t.Fatal(err)
			}
			s.post(a, base+"/push/devices/"+got["device"]+"/off", nil, nil)
			return tenant
		}
		icon := browserSub(t)
		icon["home"] = true
		if got := home(icon); got != s.tenant {
			t.Errorf("an iPhone icon's device has home business %q, want this one %q", got, s.tenant)
		}
		if got := home(browserSub(t)); got != "" {
			t.Errorf("a plain browser's device has home business %q, want none (it serves every business)", got)
		}
	})

	t.Run("turn off", func(t *testing.T) {
		code, _, h := s.post(a, base+"/push/devices/"+device+"/off", url.Values{"why": {"turned_off"}}, nil)
		if code != http.StatusSeeOther || h.Get("Location") != me || s.endedWhy(device) != "turned_off" {
			t.Errorf("turn off = %d to %q, row %q", code, h.Get("Location"), s.endedWhy(device))
		}
		if _, got := s.postJSON(a, base+"/push/hello", map[string]string{"endpoint": endpoint}, nil); got["state"] != "off" {
			t.Errorf("hello after turning off = %v, want off", got)
		}
	})

	t.Run("logging out ends the device", func(t *testing.T) {
		c := s.logIn(priya)
		_, got := s.postJSON(c, base+"/push/devices", browserSub(t), nil)
		if code, _, _ := s.post(c, base+"/logout", nil, nil); code != http.StatusSeeOther {
			t.Fatalf("logout = %d", code)
		}
		if why := s.endedWhy(got["device"]); why != "logged_out" {
			t.Errorf("after logging out the device = %q, want logged_out", why)
		}
		c = s.logIn(priya)
		_, got = s.postJSON(c, base+"/push/devices", browserSub(t), nil)
		if code, _, _ := s.post(c, base+"/logout-everywhere", nil, nil); code != http.StatusSeeOther {
			t.Fatalf("logout everywhere = %d", code)
		}
		if why := s.endedWhy(got["device"]); why != "logged_out_everywhere" {
			t.Errorf("after logging out everywhere the device = %q, want logged_out_everywhere", why)
		}
	})
}

// TestGoBackAfterLogin: a page opened without a login (a phone note's link) comes back after the
// login, and after choosing a new password; a "go back to" address outside the business is never
// followed.
func TestGoBackAfterLogin(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	item := base + "/items/00000000-0000-0000-0000-000000000007"
	loginAt := base + "/login?next=" + url.QueryEscape(item)

	code, _, h := s.get(s.browser(), item)
	if code != http.StatusSeeOther || h.Get("Location") != loginAt {
		t.Fatalf("an item without a login = %d to %q, want %q", code, h.Get("Location"), loginAt)
	}
	_, page, _ := s.get(s.browser(), loginAt)
	if !strings.Contains(page, `<input type="hidden" name="next" value="`+item+`">`) {
		t.Errorf("the login page doesn't keep the address:\n%s", page)
	}
	// The same for a made-up business address, so the page still can't tell them apart (D29).
	_, fake, _ := s.get(s.browser(), "/t/no-such-firm/login?next="+url.QueryEscape("/t/no-such-firm/items/00000000-0000-0000-0000-000000000007"))
	if strings.ReplaceAll(fake, "no-such-firm", s.slug) != page {
		t.Error("with a go-back address, a real and a made-up business address give different login pages")
	}

	logIn := func(c *http.Client, next string) string {
		t.Helper()
		_, _, h := s.post(c, base+"/login", url.Values{"email": {s.mail(arjun)}, "password": {s.passwords[arjun]}, "next": {next}}, nil)
		return h.Get("Location")
	}
	c := s.browser()
	if to := logIn(c, item); to != item {
		t.Fatalf("logging in with the item's address went to %q, want the item", to)
	}
	if code, _, h := s.get(c, item); code == http.StatusSeeOther && strings.Contains(h.Get("Location"), "/login") {
		t.Errorf("after the login the item still sends to the login page")
	}
	if code, _, h := s.get(c, loginAt); code != http.StatusSeeOther || h.Get("Location") != item {
		t.Errorf("the login page when already logged in = %d to %q, want the item", code, h.Get("Location"))
	}

	t.Run("outside addresses are refused", func(t *testing.T) {
		for _, next := range []string{"https://evil.example/", "//evil.example/t/" + s.slug + "/", "/t/" + s.slug + "//evil.example",
			"/t/other-firm/items/x", "/t/" + s.slug + "/../other-firm/", "/t/" + s.slug + "/items/x?y=https://evil.example",
			"/t/" + s.slug + `/items\x`, "javascript:alert(1)", "/t/" + s.slug + "/items/%2e%2e/x"} {
			if to := logIn(s.browser(), next); to != base+"/" {
				t.Errorf("go back to %q went to %q, want My Work", next, to)
			}
			_, page, _ := s.get(s.browser(), base+"/login?next="+url.QueryEscape(next))
			if strings.Contains(page, `name="next"`) {
				t.Errorf("the login page keeps %q", next)
			}
		}
	})

	t.Run("it survives choosing a new password", func(t *testing.T) {
		s.temporaryPassword(arjun)
		c := s.browser()
		if to := logIn(c, item); to != item {
			t.Fatalf("login = to %q", to)
		}
		pw := base + "/password?next=" + url.QueryEscape(item)
		code, _, h := s.get(c, item)
		if code != http.StatusSeeOther || h.Get("Location") != pw {
			t.Fatalf("with a temporary password the item = %d to %q, want %q", code, h.Get("Location"), pw)
		}
		_, page, _ := s.get(c, pw)
		if !strings.Contains(page, `<input type="hidden" name="then" value="`+item+`">`) {
			t.Errorf("the password page doesn't keep the address:\n%s", page)
		}
		code, _, h = s.post(c, base+"/password", url.Values{"current": {s.passwords[arjun]}, "next": {"arjun-own-pass-1"},
			"again": {"arjun-own-pass-1"}, "then": {item}}, nil)
		if code != http.StatusSeeOther || h.Get("Location") != item {
			t.Errorf("choosing a password = %d to %q, want the item", code, h.Get("Location"))
		}
		s.passwords[arjun] = "arjun-own-pass-1"
		code, _, h = s.post(s.logIn(arjun), base+"/password", url.Values{"current": {s.passwords[arjun]}, "next": {"arjun-own-pass-2"},
			"again": {"arjun-own-pass-2"}, "then": {"https://evil.example/"}}, nil)
		if code != http.StatusSeeOther || h.Get("Location") != base+"/" {
			t.Errorf("a password change with an outside address = %d to %q, want My Work", code, h.Get("Location"))
		}
		s.passwords[arjun] = "arjun-own-pass-2"
	})
}

// TestPhoneNoteLimits: one login can turn phone notes on only so often (each turn-on can add a row,
// and rows are kept), and say hello only so often; another login is not held up by it.
func TestPhoneNoteLimits(t *testing.T) {
	s, _ := newSite(t).withPush(t)
	base := "/t/" + s.slug
	a, p := s.logIn(arjun), s.logIn(priya)
	for i := 0; i < 20; i++ {
		if code, got := s.postJSON(a, base+"/push/devices", browserSub(t), nil); code != http.StatusOK {
			t.Fatalf("turn on %d = %d %v", i+1, code, got)
		}
	}
	if code, got := s.postJSON(a, base+"/push/devices", browserSub(t), nil); code != http.StatusTooManyRequests || got["error"] == "" {
		t.Errorf("the 21st turn on in an hour = %d %v, want 429 with plain words", code, got)
	}
	if code, _ := s.postJSON(p, base+"/push/devices", browserSub(t), nil); code != http.StatusOK {
		t.Errorf("another login's turn on = %d, want 200", code)
	}
	hello := map[string]string{"endpoint": "https://fcm.googleapis.com/fcm/send/test-" + rand.Text()}
	for i := 0; i < 60; i++ {
		if code, got := s.postJSON(a, base+"/push/hello", hello, nil); code != http.StatusOK {
			t.Fatalf("hello %d = %d %v", i+1, code, got)
		}
	}
	if code, _ := s.postJSON(a, base+"/push/hello", hello, nil); code != http.StatusTooManyRequests {
		t.Errorf("the 61st hello in a minute = %d, want 429", code)
	}
}
