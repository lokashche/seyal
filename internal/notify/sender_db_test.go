//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package notify_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/notify"
	"seyal/internal/push"
	"seyal/internal/workitem"
)

// toDev makes one hand-over from Ben to Dev and returns the item: Dev's phone note waits to go. (Making
// the item tells Ben and Dev about it first; that note goes with the fake's answers as they are.)
func (f *firm) toDev() workitem.Item {
	f.t.Helper()
	it := f.heldBy(1, "ben")[0]
	f.passOn(it.ID, "ben", "dev")
	return it
}

// only answers status (and Retry-After) for the device push address endpoint, and 201 for the others.
func only(endpoint string, status int, retryAfter string) func(push.Device) (int, string) {
	return func(d push.Device) (int, string) {
		if d.Endpoint == endpoint {
			return status, retryAfter
		}
		return 201, ""
	}
}

// endDevice ends the device id (as the owner), as turning phone notes off on it would.
func (f *firm) endDevice(id string) {
	f.t.Helper()
	if _, err := f.owner.Exec(f.ctx, `UPDATE push_devices SET ended_at = now(), ended_why = 'gone' WHERE id = $1 AND ended_at IS NULL`, id); err != nil {
		f.t.Fatal(err)
	}
}

// failUpdates makes every change to the device row id fail, until undo runs: a stand-in for a
// database error just after a push service answered. It is a trigger (made as the owner) that looks
// at that one row only, so other tests' devices are not touched.
func (f *firm) failUpdates(id string) (undo func()) {
	f.t.Helper()
	name := "test_fail_device_" + strings.ReplaceAll(id, "-", "")
	for _, q := range []string{
		`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test: this device row can''t change'; END $$`,
		`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON push_devices FOR EACH ROW WHEN (OLD.id = '` + id + `'::uuid) EXECUTE FUNCTION ` + name + `()`,
	} {
		if _, err := f.owner.Exec(f.ctx, q); err != nil {
			f.t.Fatal(err)
		}
	}
	done := false
	undo = func() {
		if done {
			return
		}
		done = true
		for _, q := range []string{`DROP TRIGGER IF EXISTS ` + name + ` ON push_devices`, `DROP FUNCTION IF EXISTS ` + name + `()`} {
			if _, err := f.owner.Exec(context.Background(), q); err != nil {
				f.t.Error(err)
			}
		}
	}
	f.t.Cleanup(undo) // also when the test stops early
	return undo
}

// devNote reads Dev's latest notice: its tries, when it is tried next, and what became of it.
func (f *firm) devNote() (tries int, next *time.Time, result string) {
	f.t.Helper()
	f.do(func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT tries, next_try_at, coalesce(result, '') FROM notices
			WHERE tenant_id = current_tenant() AND person_id::text = $1 ORDER BY id DESC LIMIT 1`, f.people["dev"]).
			Scan(&tries, &next, &result)
	})
	return tries, next, result
}

// passLogged runs a pass that may log, and returns (and forgets) what it logged.
func (f *firm) passLogged() string {
	f.t.Helper()
	f.r.Pass(f.ctx)
	got := f.log.String()
	f.log.Reset()
	return got
}

// noAddress checks that no device push address of the firm is in what the reader logged.
func (f *firm) noAddress(logged string) {
	f.t.Helper()
	for key, e := range f.endpoints {
		if strings.Contains(logged, e) || strings.Contains(logged, "test-"+key) {
			f.t.Errorf("the log holds %s's device push address: %s", key, logged)
		}
	}
}

// TestReplyCodes: what each answer from a fake push service does to Dev's phone note and his device
// (design 2.1, "The answer").
func TestReplyCodes(t *testing.T) {
	f := newFirm(t)
	posts := func() int {
		f.out.mu.Lock()
		defer f.out.mu.Unlock()
		return f.out.posts
	}

	t.Run("201: sent once, and the device is marked as working", func(t *testing.T) {
		job := f.toDev()
		want(t, "201", f.pass(), "Dev: New work from Ben: "+job.Number)
		var ok *time.Time
		if err := f.owner.QueryRow(f.ctx, `SELECT last_ok_at FROM push_devices WHERE id = $1`, f.devices["dev"]).Scan(&ok); err != nil || ok == nil {
			t.Errorf("last_ok_at = %v, %v; want set", ok, err)
		}
		f.clock = f.clock.Add(time.Hour)
		before := posts()
		want(t, "an hour later", f.pass())
		if posts() != before {
			t.Error("a phone note a push service took was posted again")
		}
	})

	t.Run("500: tried again after 1, 5 and 30 minutes, then given up", func(t *testing.T) {
		f.toDev()
		f.out.answering(only(f.endpoints["dev"], 500, ""))
		before := posts()
		for i, wait := range []time.Duration{0, time.Minute, 5 * time.Minute, 30 * time.Minute} {
			if i > 0 {
				f.clock = f.clock.Add(wait - time.Second)
				if logged := f.passLogged(); logged != "" {
					t.Errorf("try %d: posted a second early (%s)", i+1, logged)
				}
				f.clock = f.clock.Add(time.Second)
			}
			logged := f.passLogged()
			f.noAddress(logged)
			tries, _, result := f.devNote()
			if posts() != before+i+1 || tries != i+1 {
				t.Fatalf("try %d: %d posts, tries %d (log %q)", i+1, posts()-before, tries, logged)
			}
			if i < 3 && (result != "" || !strings.Contains(logged, "answered 500") || !strings.Contains(logged, "waits")) {
				t.Errorf("try %d: result %q, log %q; want it waiting", i+1, result, logged)
			}
			if i == 3 && (result != notify.Failed || !strings.Contains(logged, "given up")) {
				t.Errorf("the last try: result %q, log %q; want it given up", result, logged)
			}
		}
		f.clock = f.clock.Add(time.Hour)
		f.passLogged()
		if posts() != before+notify.MaxTries {
			t.Errorf("%d posts in all; want %d", posts()-before, notify.MaxTries)
		}
		if why := f.endedWhy(f.devices["dev"]); why != "" {
			t.Errorf("a push service with a problem ended Dev's device (%s)", why)
		}
		f.out.answering(nil)
	})

	t.Run("429: waits as long as Retry-After says", func(t *testing.T) {
		job := f.toDev()
		f.out.answering(only(f.endpoints["dev"], 429, "180"))
		f.passLogged()
		if _, next, result := f.devNote(); result != "" || next == nil || next.Sub(f.clock.Add(3*time.Minute)).Abs() > time.Millisecond {
			t.Errorf("after 429: next try %v, result %q; want in 3 minutes", next, result)
		}
		f.out.answering(nil)
		f.clock = f.clock.Add(2 * time.Minute)
		want(t, "2 minutes later", f.pass())
		f.clock = f.clock.Add(time.Minute)
		want(t, "3 minutes later", f.pass(), "Dev: New work from Ben: "+job.Number)
	})

	t.Run("400: logged once, not tried again, the device stays", func(t *testing.T) {
		f.toDev()
		f.out.answering(only(f.endpoints["dev"], 400, ""))
		before := posts()
		logged := f.passLogged()
		if !strings.Contains(logged, "answered 400") || strings.Count(logged, "refused a phone note") != 1 {
			t.Errorf("log %q; want one line about the 400", logged)
		}
		f.noAddress(logged)
		f.clock = f.clock.Add(time.Hour)
		if logged := f.passLogged(); logged != "" {
			t.Errorf("an hour later the reader logged %q", logged)
		}
		if _, _, result := f.devNote(); posts() != before+1 || result != notify.Failed {
			t.Errorf("%d posts, result %q; want one post, failed", posts()-before, result)
		}
		if why := f.endedWhy(f.devices["dev"]); why != "" {
			t.Errorf("a 400 ended Dev's device (%s)", why)
		}
		f.out.answering(nil)
	})

	for _, code := range []int{404, 410} {
		t.Run(fmt.Sprintf("%d: a device that is gone ends at once", code), func(t *testing.T) {
			f.toDev()
			f.out.answering(only(f.endpoints["dev"], code, ""))
			logged := f.passLogged()
			f.noAddress(logged)
			if why := f.endedWhy(f.devices["dev"]); why != "gone" {
				t.Errorf("%d: Dev's device ended %q; want gone", code, why)
			}
			if _, _, result := f.devNote(); result != notify.Failed {
				t.Errorf("%d: result %q; want failed (no device took it)", code, result)
			}
			// Dev has no phone now: his next phone note is not made into a post at all.
			f.out.answering(nil)
			f.toDev()
			f.passLogged()
			if _, _, result := f.devNote(); result != notify.NoDevice {
				t.Errorf("%d: with no device, result %q; want no_device", code, result)
			}
			f.endpoints["dev"], f.devices["dev"] = f.addDevice("dev") // Dev taps Turn on again
		})
	}

	for _, code := range []int{401, 403} {
		t.Run(fmt.Sprintf("%d: Seyal's key refused for one device: it ends at the end of the pass", code), func(t *testing.T) {
			job := f.toDev()
			f.out.answering(only(f.endpoints["dev"], code, ""))
			logged := f.passLogged()
			f.noAddress(logged)
			if why := f.endedWhy(f.devices["dev"]); why != "gone" || strings.Count(logged, "a device ended: its push service no longer accepts Seyal's key") != 1 {
				t.Errorf("%d: Dev's device ended %q, log %q; want gone", code, why, logged)
			}
			f.out.answering(nil)
			f.endpoints["dev"], f.devices["dev"] = f.addDevice("dev") // Dev taps Turn on again
			f.clock = f.clock.Add(time.Minute)
			want(t, "on the next try", f.pass(), "Dev: New work from Ben: "+job.Number)
		})
	}

	t.Run("two phones: one taking it is enough", func(t *testing.T) {
		job := f.toDev()
		second, secondID := f.addDevice("dev")
		f.out.answering(only(second, 503, ""))
		want(t, "one phone up, one push service down", f.pass(), "Dev: New work from Ben: "+job.Number)
		f.clock = f.clock.Add(time.Hour)
		f.out.answering(nil)
		want(t, "an hour later", f.pass())
		f.endDevice(secondID)
	})

	t.Run("400 on one phone, 503 on the other: it waits, and the retry goes to both", func(t *testing.T) {
		job := f.toDev()
		second, secondID := f.addDevice("dev")
		first := f.endpoints["dev"]
		f.out.answering(func(d push.Device) (int, string) {
			switch d.Endpoint {
			case first:
				return 400, ""
			case second:
				return 503, ""
			}
			return 201, ""
		})
		before := posts()
		logged := f.passLogged()
		if tries, _, result := f.devNote(); posts() != before+2 || tries != 1 || result != "" {
			t.Errorf("%d posts, tries %d, result %q; want two posts, and the note waiting", posts()-before, tries, result)
		}
		if strings.Count(logged, "refused a phone note (answered 400)") != 1 || !strings.Contains(logged, "brings no retry") ||
			strings.Contains(logged, "not posted to that device again") {
			t.Errorf("log %q; want one honest line about the 400", logged)
		}
		f.noAddress(logged)
		// The second phone's push service is back: the retry goes to both phones, and the second takes it.
		f.out.answering(only(first, 400, ""))
		f.clock = f.clock.Add(time.Minute)
		if logged := f.passLogged(); strings.Count(logged, "answered 400") != 1 {
			t.Errorf("on the retry: log %q; want the 400 again, once", logged)
		}
		want(t, "a minute later", f.told(), "Dev: New work from Ben: "+job.Number)
		if posts() != before+4 {
			t.Errorf("%d posts in all; want 4 (both phones, twice)", posts()-before)
		}
		f.out.answering(nil)
		f.endDevice(secondID)
	})

	t.Run("a device row that can't change after a 201 never undoes the mark", func(t *testing.T) {
		job := f.toDev()
		laptop, laptopID := f.addDevice("dev")
		f.out.answering(only(laptop, 410, "")) // the phone takes it; the laptop's address is gone
		undo := f.failUpdates(laptopID)        // ... and ending the laptop's row fails
		before := posts()
		logged := f.passLogged()
		if !strings.Contains(logged, "can't save what the push services said") {
			t.Errorf("log %q; want the failed device change logged", logged)
		}
		f.noAddress(logged)
		if _, _, result := f.devNote(); result != notify.Sent || posts() != before+2 {
			t.Errorf("result %q, %d posts; want sent, after one post to each device", result, posts()-before)
		}
		want(t, "the phone took it", f.told(), "Dev: New work from Ben: "+job.Number)
		f.clock = f.clock.Add(time.Hour)
		f.out.answering(nil)
		want(t, "an hour later", f.pass())
		if posts() != before+2 {
			t.Errorf("the note the phone took was posted again: %d posts", posts()-before)
		}
		undo()
		f.endDevice(laptopID)
	})
	f.verified() // phone notes add no ledger events
}

// TestKeyGuard: when push services refuse Seyal's key for most devices, Seyal ends none of them and
// logs "check the SEYAL_PUSH keys" (a mistake in .env must not end everyone's phone notes).
func TestKeyGuard(t *testing.T) {
	f := newFirm(t)
	f.out.answering(func(push.Device) (int, string) { return 403, "" }) // a wrong key: every push service refuses
	f.startJob()                                                        // Ben and Dev get "New work for Operations"
	logged := f.passLogged()
	if !strings.Contains(logged, "check the SEYAL_PUSH keys") || !strings.Contains(logged, "2 of 2 devices") {
		t.Errorf("log %q; want the key warning", logged)
	}
	f.noAddress(logged)
	for _, key := range []string{"ben", "dev"} {
		if why := f.endedWhy(f.devices[key]); why != "" {
			t.Errorf("%s's device ended (%s)", key, why)
		}
	}
	// The key is put right: the waiting notes go on their next try.
	f.out.answering(nil)
	f.clock = f.clock.Add(time.Minute)
	if got := f.pass(); len(got) != 2 {
		t.Errorf("after the key was put right: %q; want Ben's and Dev's notes", got)
	}

	// The key works now, but one push service refuses for most devices at once: still nobody's ends.
	f.out.answering(func(push.Device) (int, string) { return 401, "" })
	f.startJob()
	if logged := f.passLogged(); !strings.Contains(logged, "check the SEYAL_PUSH keys") {
		t.Errorf("log %q; want the key warning", logged)
	}
	for _, key := range []string{"ben", "dev"} {
		if why := f.endedWhy(f.devices[key]); why != "" {
			t.Errorf("%s's device ended (%s)", key, why)
		}
	}

	// A server that has just started, whose key no push service has taken yet: even one refusal (Dev's
	// phone, the only one posted to) ends nothing.
	g := newFirm(t)
	g.toDev()
	g.r = &notify.Reader{DB: g.app, Log: log.New(&g.log, "", 0), Push: true, Out: g.out, List: g.r.List, Now: g.r.Now}
	g.out.answering(only(g.endpoints["dev"], 403, ""))
	if logged := g.passLogged(); !strings.Contains(logged, "check the SEYAL_PUSH keys") || !strings.Contains(logged, "1 of 1 devices") {
		t.Errorf("a server that has just started: log %q; want the key warning", logged)
	}
	if why := g.endedWhy(g.devices["dev"]); why != "" {
		t.Errorf("before any push service took a note, one refusal ended Dev's device (%s)", why)
	}

	// Each push service checks the key on its own. Here Google's takes it and Apple's refuses it (for
	// example a SEYAL_PUSH_CONTACT that only Apple doesn't accept): Google's 201s prove nothing for
	// Apple, so Dev's iPhone, the one device refused in its pass, doesn't end.
	h := newFirm(t)
	h.r = &notify.Reader{DB: h.app, Log: log.New(&h.log, "", 0), Push: true, Out: h.out, List: h.r.List, Now: h.r.Now}
	iphone, iphoneID := h.addDeviceAt("dev", "https://web.push.apple.com/")
	h.out.answering(only(iphone, 403, ""))
	h.startJob() // Ben's and Dev's Android phones take it; Dev's iPhone refuses
	logged = h.passLogged()
	if !strings.Contains(logged, "check the SEYAL_PUSH keys") || !strings.Contains(logged, "1 of 3 devices") ||
		strings.Contains(logged, "a device ended") {
		t.Errorf("Google took it, Apple refused: log %q; want the key warning, and no device ended", logged)
	}
	h.noAddress(logged)
	if why := h.endedWhy(iphoneID); why != "" {
		t.Errorf("a 201 from Google ended Dev's iPhone on Apple's 403 (%s)", why)
	}
	// Once Apple's push service took a note, a later 403 from it is about that one device: it ends.
	h.out.answering(nil)
	h.startJob()
	h.pass()
	h.out.answering(only(iphone, 403, ""))
	h.startJob()
	logged = h.passLogged()
	if why := h.endedWhy(iphoneID); why != "gone" || !strings.Contains(logged, "a device ended") ||
		strings.Contains(logged, "check the SEYAL_PUSH keys") {
		t.Errorf("after Apple took a note: the iPhone ended %q, log %q; want it ended", why, logged)
	}
	h.noAddress(logged)
}

// TestKeyGuardOneLogin: Seyal can't check which key a browser was asked to use, so Dev can turn on
// five devices with a key of his own, and every push service refuses them all. The guard counts
// people, not devices: Dev's five can't hold it or raise the "check the SEYAL_PUSH keys" alarm, and
// they end like any refused device. Ben's phone note still goes.
func TestKeyGuardOneLogin(t *testing.T) {
	f := newFirm(t)
	f.startJob() // Ben's and Dev's phones take it: Google's push service has taken a note with Seyal's key
	f.pass()
	devs := map[string]string{f.endpoints["dev"]: f.devices["dev"]}
	for range 4 {
		e, id := f.addDevice("dev")
		devs[e] = id
	}
	refuse := func(d push.Device) (int, string) {
		if _, ok := devs[d.Endpoint]; ok {
			return 403, ""
		}
		return 201, ""
	}
	f.out.answering(refuse)
	f.startJob() // Ben's phone takes it; Dev's five devices refuse (5 of 6 devices, 1 of 2 people)
	logged := f.passLogged()
	f.noAddress(logged)
	if strings.Contains(logged, "check the SEYAL_PUSH keys") || strings.Count(logged, "a device ended") != 5 {
		t.Errorf("log %q; want Dev's five devices ended, and no key warning", logged)
	}
	for _, id := range devs {
		if why := f.endedWhy(id); why != "gone" {
			t.Errorf("one of Dev's refused devices ended %q; want gone", why)
		}
	}
	if why := f.endedWhy(f.devices["ben"]); why != "" {
		t.Errorf("Ben's phone ended (%s)", why)
	}

	// A pass with only Dev's note (a poke just after a hand-over): his new five refuse too, 5 of 5
	// devices, but still one person.
	f.toDev()
	devs = map[string]string{}
	for range 5 {
		e, id := f.addDevice("dev")
		devs[e] = id
	}
	logged = f.passLogged()
	if strings.Contains(logged, "check the SEYAL_PUSH keys") || strings.Count(logged, "a device ended") != 5 {
		t.Errorf("only Dev's note: log %q; want his five devices ended, and no key warning", logged)
	}
	f.out.answering(nil)
}

// TestNoDoubleSend: two copies of the app deliver at the same moment. The first takes Dev's notice
// (FOR UPDATE SKIP LOCKED); the second skips it rather than waiting, and Dev's phone gets one post.
func TestNoDoubleSend(t *testing.T) {
	f := newFirm(t)
	job := f.toDev()
	if _, err := f.r.Read(f.ctx, f.tenant); err != nil {
		t.Fatal(err)
	}
	other := &notify.Reader{DB: f.app, Log: log.New(&f.log, "", 0), Push: true, Out: f.out, List: f.r.List, Now: f.r.Now}
	sending, release := make(chan struct{}), make(chan struct{})
	f.out.whenSending(func(call int) {
		if call == 1 {
			close(sending)
			<-release // the first copy's push service is slow
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.r.Deliver(f.ctx, f.tenant)
		done <- err
	}()
	<-sending
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	n, err := other.Deliver(ctx, f.tenant)
	cancel()
	if err != nil || n != 0 {
		t.Errorf("the second copy delivered %d (%v); want none, at once", n, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.out.whenSending(nil)
	want(t, "both copies delivered", f.told(), "Dev: New work from Ben: "+job.Number)
	f.clock = f.clock.Add(time.Hour)
	want(t, "an hour later", f.pass())
}

// TestAddressNeverLogged: a sender whose error holds the device push address (as Go's own errors do)
// never gets it into the log: only fixed short reasons are written.
func TestAddressNeverLogged(t *testing.T) {
	f := newFirm(t)
	f.toDev()
	f.out.mu.Lock()
	f.out.err = errors.New(`Post "` + f.endpoints["dev"] + `": dial tcp 10.0.0.1:443: connect: connection refused`)
	f.out.mu.Unlock()
	logged := f.passLogged()
	if !strings.Contains(logged, "could not lock or post it") {
		t.Errorf("log %q; want the fixed reason", logged)
	}
	f.noAddress(logged)
	if strings.Contains(logged, "10.0.0.1") || strings.Contains(logged, "dial tcp") {
		t.Errorf("Go's error text reached the log: %s", logged)
	}
}

// TestPoke: Ben passes a job to Dev and pokes the reader, as a web request does after a change. Dev's
// phone gets the note within seconds, not at the next minute's pass. Poke never blocks.
func TestPoke(t *testing.T) {
	f := newFirm(t)
	job := f.heldBy(1, "ben")[0]
	r := notify.New(f.app, log.New(&f.log, "", 0), f.out)
	r.List, r.Now = f.r.List, f.r.Now
	ctx, cancel := context.WithCancel(f.ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.Run(ctx, time.Hour) // the minute's pass never comes in this test
	}()
	defer func() {
		cancel()
		<-stopped
		if f.log.Len() > 0 {
			t.Errorf("the reader logged: %s", f.log.String())
		}
	}()

	start := time.Now()
	f.passOn(job.ID, "ben", "dev")
	for range 100 {
		r.Poke(f.tenant.ID) // a burst of pokes is one pass, and none waits
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("100 pokes took %s", d)
	}
	for time.Since(start) < 10*time.Second && len(f.notes()) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	took := time.Since(start)
	want(t, "Ben passes to Dev", f.told(), "Dev: New work from Ben: "+job.Number)
	if took > 5*time.Second {
		t.Errorf("Dev's phone got it after %s; want within seconds", took)
	}
	t.Logf("Dev's phone got it %.1f seconds after the hand-over", took.Seconds())

	// With phone notes off on the server, a poke does nothing (and doesn't wait for a reader that
	// isn't running).
	notify.New(f.app, log.New(&f.log, "", 0), nil).Poke(f.tenant.ID)
}
