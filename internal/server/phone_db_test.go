//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Phones show one screen at a time (the layout itself is CSS); these check what the pages tell the
// layout: which screen is meant, the bar at the bottom, and messages inside their pane.
func TestPhoneScreens(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	tabCurrent := regexp.MustCompile(`<nav class="tabbar"[^>]*>(?s:.*?)<a href="[^"]*" aria-current="page">[^<]*<svg(?s:.*?)</svg>([^<]+)</a>`)
	current := func(page string) string {
		if m := tabCurrent.FindStringSubmatch(page); m != nil {
			return m[1]
		}
		return "(none)"
	}

	t.Run("My Work is the list; Tell Seyal is its own screen", func(t *testing.T) {
		_, home, _ := s.get(a, base+"/")
		if !strings.Contains(home, `<div class="app">`) || current(home) != "My Work" {
			t.Errorf("home: want the list screen with My Work current, got tab %q", current(home))
		}
		if !strings.Contains(home, `&amp;view=tell"`) {
			t.Error("the Tell Seyal button doesn't open Tell Seyal on its own")
		}
		_, tell, _ := s.get(a, base+"/?view=tell")
		if !strings.Contains(tell, `<div class="app tell-view">`) || current(tell) != "Tell Seyal" {
			t.Errorf("?view=tell: want the Tell Seyal screen, got tab %q", current(tell))
		}
	})

	t.Run("after sending, Tell Seyal stays on screen with Seyal's reply inside it", func(t *testing.T) {
		_, home, _ := s.get(a, base+"/")
		pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
		if pv == nil {
			t.Fatal("no process to start")
		}
		code, page, _ := s.post(a, base+"/tell", url.Values{"playbook": {pv[1]}, "text": {"Hello"}}, nil)
		if code != http.StatusOK || !strings.Contains(page, `<div class="app tell-view">`) {
			t.Fatalf("greeting = %d; want the Tell Seyal screen again", code)
		}
		pane := page[strings.Index(page, `aria-label="Tell Seyal"`):]
		if i := strings.Index(pane, `class="banner notice"`); i < 0 || i > strings.Index(pane, `id="thread"`) {
			t.Error("Seyal's reply isn't shown inside the Tell Seyal pane")
		}
	})

	t.Run("a work item has Chat | Details and a way back", func(t *testing.T) {
		_, home, _ := s.get(a, base+"/")
		pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
		item := s.send(a, base+"/tell", url.Values{"playbook": {pv[1]}, "text": {"New monthly books job for Sundaram"}})
		for _, want := range []string{`class="app has-item"`, `class="item-bar"`, `data-show="chat"`, `data-show="details"`, `class="back"`} {
			if !strings.Contains(item, want) {
				t.Errorf("item page is missing %s", want)
			}
		}
	})

	t.Run("Settings is in the bar for admins only", func(t *testing.T) {
		_, mine, _ := s.get(a, base+"/")
		if strings.Contains(mine, `>Settings</a>`) {
			t.Error("a non-admin has Settings in the bar")
		}
		l := s.logIn(lakshmi)
		_, page, _ := s.get(l, base+"/settings/people")
		if current(page) != "Settings" {
			t.Errorf("Settings page: current tab %q", current(page))
		}
	})

	t.Run("a new person's password shows inside the panel, so the page doesn't move", func(t *testing.T) {
		l := s.logIn(lakshmi)
		_, page, _ := s.post(l, base+"/settings/people",
			url.Values{"name": {"Neha Joshi"}, "email": {s.mail("neha")}, "role": {"sales"}}, nil)
		panel := strings.Index(page, `<section class="panel">`)
		notice := strings.Index(page, `class="banner notice"`)
		if panel < 0 || notice < panel {
			t.Errorf("the notice is outside the panel (panel at %d, notice at %d)", panel, notice)
		}
	})

	t.Run("your name and the Me tab open your profile", func(t *testing.T) {
		_, home, _ := s.get(a, base+"/")
		if !strings.Contains(home, `class="me-name" href="`+base+`/me"`) || !strings.Contains(home, `<a href="`+base+`/me"`) {
			t.Error("your name or the Me tab doesn't open the profile")
		}
		code, me, _ := s.get(a, base+"/me")
		for _, want := range []string{"Arjun Mehta", s.mail(arjun), "Kaveri Associates (demo)", "Sales", `href="` + base + `/password"`,
			`action="` + base + `/logout"`, `action="` + base + `/logout-everywhere"`} {
			if !strings.Contains(me, want) {
				t.Errorf("the Me page is missing %s", want)
			}
		}
		if code != http.StatusOK || current(me) != "Me" {
			t.Errorf("Me page = %d, tab %q", code, current(me))
		}
		if _, pw, _ := s.get(a, base+"/password"); !strings.Contains(pw, `href="`+base+`/me">← Back to Me`) || current(pw) != "Me" {
			t.Error("the password page doesn't lead back to Me")
		}
	})
}

func TestLogoutEverywhere(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	phone, laptop, other := s.logIn(arjun), s.logIn(arjun), s.logIn(priya)

	code, _, h := s.post(phone, base+"/logout-everywhere", nil, nil)
	if code != http.StatusSeeOther || h.Get("Location") != base+"/login" {
		t.Fatalf("log out everywhere = %d to %q", code, h.Get("Location"))
	}
	for name, c := range map[string]*http.Client{"the phone": phone, "the laptop": laptop} {
		if code, _, _ := s.get(c, base+"/"); code != http.StatusSeeOther {
			t.Errorf("%s is still logged in (%d)", name, code)
		}
	}
	if code, _, _ := s.get(other, base+"/"); code != http.StatusOK {
		t.Errorf("someone else was logged out too (%d)", code)
	}
}
