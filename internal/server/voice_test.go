// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"testing"
	"time"

	"seyal/internal/ai"
	"seyal/internal/auth"
	"seyal/internal/channel"
	"seyal/internal/seal"
)

func TestSpeechSlots(t *testing.T) {
	s := newSpeechSlots(3, 2)
	if !s.take("a", "login1") || !s.take("a", "login2") {
		t.Fatal("a business can't use its share")
	}
	if s.take("a", "login3") {
		t.Error("one business took more than its share")
	}
	if !s.take("b", "login3") {
		t.Fatal("another business can't use a free slot")
	}
	if s.take("c", "login4") {
		t.Error("more calls at once than the server allows")
	}
	s.give("a", "login1")
	if !s.take("c", "login4") {
		t.Error("a slot given back can't be used")
	}
	s.give("a", "login2")
	s.give("b", "login3")
	s.give("c", "login4")
	if s.busy != 0 || len(s.perBusiness) != 0 || len(s.perLogin) != 0 {
		t.Errorf("after every give: busy %d, per business %v, per login %v", s.busy, s.perBusiness, s.perLogin)
	}
}

// One login has one call at a time: in its own business, and also in another business it belongs to
// (so one login in two businesses can't hold every slot on the server).
func TestSpeechSlotsOnePerLogin(t *testing.T) {
	s := newSpeechSlots(4, 2)
	if !s.take("a", "login1") {
		t.Fatal("a login can't make a call")
	}
	if s.take("a", "login1") {
		t.Error("one login holds two calls in one business")
	}
	if s.take("b", "login1") {
		t.Error("one login holds calls in two businesses at once")
	}
	if !s.take("a", "login2") {
		t.Error("another person in the business can't use its free slot")
	}
	s.give("a", "login1")
	if !s.take("b", "login1") {
		t.Error("a login can't call again after its call ended")
	}
}

// TestAudioUse: minutes of audio are counted per login (in 10 minutes and in a day) and per business
// (in a day), so one login can't send note after note to the paid service.
func TestAudioUse(t *testing.T) {
	u := newAudioUse()
	lim := audioLimits{loginShort: 600, loginDay: 1200, businessDay: 1500}
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	take := func(business, login string, seconds float64, at time.Time) (bool, bool) {
		return u.take(business, login, seconds, lim, at)
	}
	if ok, _ := take("a", "login1", 360, now); !ok {
		t.Fatal("a 6-minute note was refused")
	}
	if ok, today := take("a", "login1", 300, now.Add(time.Minute)); ok || today {
		t.Errorf("11 minutes in 10 = %v (today %v), want refused by the short limit", ok, today)
	}
	if ok, _ := take("a", "login1", 240, now.Add(2*time.Minute)); !ok {
		t.Error("10 minutes in 10 were refused")
	}
	if ok, _ := take("a", "login1", 0, now.Add(3*time.Minute)); ok {
		t.Error("a note of no measured length got past a limit already used up")
	}
	// After 10 minutes the short limit is free again; the day's 20 minutes still count.
	if ok, _ := take("a", "login1", 600, now.Add(13*time.Minute)); !ok {
		t.Error("the short limit didn't free up after 10 minutes")
	}
	if ok, today := take("a", "login1", 1, now.Add(30*time.Minute)); ok || !today {
		t.Errorf("past the login's minutes a day = %v (today %v), want refused for today", ok, today)
	}
	// Another login of the same business has its own minutes, but shares the business's.
	if ok, _ := take("a", "login2", 300, now.Add(30*time.Minute)); !ok {
		t.Error("another login was refused")
	}
	if ok, today := take("a", "login3", 1, now.Add(31*time.Minute)); ok || !today {
		t.Errorf("past the business's minutes a day = %v (today %v), want refused for today", ok, today)
	}
	if ok, _ := take("b", "login3", 300, now.Add(31*time.Minute)); !ok {
		t.Error("another business was refused")
	}
	// A day later everything counts again, and what is over a day old is forgotten.
	if ok, _ := take("a", "login1", 360, now.Add(25*time.Hour)); !ok {
		t.Error("the day's limit didn't free up after 24 hours")
	}
	if _, kept := u.sent["login login2"]; kept || len(u.sent) != 2 {
		t.Errorf("after a day, still counted: %v", u.sent)
	}
	if got := speechLimits(ai.Speech{}); got != (audioLimits{loginShort: 600, loginDay: 3600, businessDay: 14400}) {
		t.Errorf("default limits = %+v", got)
	}
	if got := speechLimits(ai.Speech{DayMinutes: 30, LoginDayMinutes: 5}); got.businessDay != 1800 || got.loginDay != 300 {
		t.Errorf("limits set in .env = %+v", got)
	}
}

// TestHeardFrom: the words of a voice note are accepted only from a token this server signed, for the
// same business and person, within an hour.
func TestHeardFrom(t *testing.T) {
	s := &Server{voiceKey: seal.NewKey()}
	me := &call{tenant: tenant{ID: "t1"}, session: auth.Session{PersonID: "p1"}}
	now := time.Now()
	heard := channel.Heard{SHA256: "abc", Model: "fake:test", Raw: "Acme Traders called"}
	sign := func(key []byte, tenant, person string, at time.Time) string {
		token, err := seal.Sign(key, heardNote{Tenant: tenant, Person: person, At: at.Unix(), Heard: heard})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	if got, err := s.heardFrom(sign(s.voiceKey, "t1", "p1", now.Add(-50*time.Minute)), me, now); err != nil || got != heard {
		t.Fatalf("a good token = %+v, %v", got, err)
	}
	for name, token := range map[string]string{
		"another business":        sign(s.voiceKey, "t2", "p1", now),
		"another person":          sign(s.voiceKey, "t1", "p2", now),
		"over an hour old":        sign(s.voiceKey, "t1", "p1", now.Add(-61*time.Minute)),
		"from the future":         sign(s.voiceKey, "t1", "p1", now.Add(5*time.Minute)),
		"another key (a restart)": sign(seal.NewKey(), "t1", "p1", now),
		"not a token":             "words",
	} {
		if _, err := s.heardFrom(token, me, now); !errors.Is(err, errHeard) {
			t.Errorf("%s: %v, want errHeard", name, err)
		}
	}
}
