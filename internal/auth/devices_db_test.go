//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/push"
)

// newDevice is a made-up browser: a device push address on Google's push service (never called: no
// test sends anything) and real lock keys, as a browser would give them.
func newDevice(t *testing.T) push.Device {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	return push.Device{
		Endpoint: "https://fcm.googleapis.com/fcm/send/test-" + rand.Text(),
		P256dh:   base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(secret),
	}
}

func (l login) turnOn(t *testing.T, token string, d push.Device) string {
	t.Helper()
	id, err := l.tryTurnOn(token, d)
	if err != nil {
		t.Fatalf("turn on: %v", err)
	}
	return id
}

func (l login) tryTurnOn(token string, d push.Device) (id string, err error) {
	return l.tryTurnOnAt(token, d, "")
}

// tryTurnOnAt turns on with a home business (an iPhone Home Screen icon of that business), or "".
func (l login) tryTurnOnAt(token string, d push.Device, home string) (id string, err error) {
	err = l.server(func(tx pgx.Tx) error {
		var err error
		id, err = auth.TurnOnDevice(context.Background(), tx, token, d, "https://seyal.kaveri-demo.example", "Chrome on Android", home)
		return err
	})
	return id, err
}

func (l login) hello(t *testing.T, token, endpoint string) auth.Hello {
	t.Helper()
	var h auth.Hello
	if err := l.server(func(tx pgx.Tx) error {
		var err error
		h, err = auth.HelloDevice(context.Background(), tx, token, endpoint)
		return err
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return h
}

// ended returns why a device ended ("" while it is on), read as the owner.
func (l login) ended(t *testing.T, id string) string {
	t.Helper()
	var why string
	if err := l.owner.QueryRow(context.Background(), `SELECT coalesce(ended_why, '') FROM push_devices WHERE id = $1`, id).
		Scan(&why); err != nil {
		t.Fatal(err)
	}
	return why
}

func (l login) devices(t *testing.T) []auth.Device {
	t.Helper()
	var list []auth.Device
	if err := l.server(func(tx pgx.Tx) error {
		var err error
		list, err = auth.Devices(context.Background(), tx, l.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return list
}

// TestTurnOnPhoneNotes: a browser turns phone notes on with checked keys; the same browser again keeps
// its row; at most five stay on per login; a session that ended can't turn one on; and nobody but
// the login can turn its device off.
func TestTurnOnPhoneNotes(t *testing.T) {
	ctx := context.Background()
	asha := newLogin(t, "Devices One")
	token := asha.logIn(t)
	d := newDevice(t)
	id := asha.turnOn(t, token, d)
	if again := asha.turnOn(t, token, d); again != id {
		t.Errorf("the same browser again = row %s, want its row %s kept", again, id)
	}
	if list := asha.devices(t); len(list) != 1 || list[0].ID != id || list[0].Label != "Chrome on Android" {
		t.Errorf("devices = %+v, want the one", list)
	}

	t.Run("the browser's details are checked", func(t *testing.T) {
		bad := map[string]push.Device{
			"an address that isn't a push service": {Endpoint: "https://push.kaveri-demo.example/x", P256dh: d.P256dh, Auth: d.Auth},
			"plain http":                           {Endpoint: "http://fcm.googleapis.com/fcm/send/x", P256dh: d.P256dh, Auth: d.Auth},
			"a key that isn't P-256":               {Endpoint: newDevice(t).Endpoint, P256dh: strings.Repeat("A", 87), Auth: d.Auth},
			"a 15-byte secret":                     {Endpoint: newDevice(t).Endpoint, P256dh: d.P256dh, Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 15))},
		}
		for name, b := range bad {
			_, err := asha.tryTurnOn(token, b)
			if !errors.Is(err, auth.ErrBadDevice) || strings.Contains(err.Error(), b.Endpoint) {
				t.Errorf("%s = %v, want ErrBadDevice without the address", name, err)
			}
		}
		if n := len(asha.devices(t)); n != 1 {
			t.Errorf("after the refusals the login has %d devices, want 1", n)
		}
	})

	t.Run("the same browser with new keys is a new row", func(t *testing.T) {
		fresh := newDevice(t)
		fresh.Endpoint = d.Endpoint
		id2 := asha.turnOn(t, token, fresh)
		if id2 == id || asha.ended(t, id) != "replaced" {
			t.Errorf("new keys: row %s, old row ended %q; want a new row and the old one replaced", id2, asha.ended(t, id))
		}
		id = id2
	})

	t.Run("a sixth device ends the oldest", func(t *testing.T) {
		ids := []string{id}
		for range 5 {
			ids = append(ids, asha.turnOn(t, token, newDevice(t)))
		}
		if asha.ended(t, ids[0]) != "too_many" || len(asha.devices(t)) != auth.MaxDevices {
			t.Errorf("six devices: the oldest ended %q, %d open; want too_many and %d", asha.ended(t, ids[0]),
				len(asha.devices(t)), auth.MaxDevices)
		}
	})

	t.Run("an ended session can't turn one on", func(t *testing.T) {
		old := asha.logIn(t)
		if err := asha.server(func(tx pgx.Tx) error { return auth.LogOut(ctx, tx, old) }); err != nil {
			t.Fatal(err)
		}
		_, err := asha.tryTurnOn(old, newDevice(t))
		wantNoSession(t, "turning on after logging out", err)
	})

	t.Run("only the login can turn its device off", func(t *testing.T) {
		ben := newLogin(t, "Devices Other")
		mine := asha.turnOn(t, token, newDevice(t))
		if err := ben.server(func(tx pgx.Tx) error { return auth.TurnOffDevice(ctx, tx, ben.id, mine, "turned_off") }); err != nil {
			t.Fatal(err)
		}
		if why := asha.ended(t, mine); why != "" {
			t.Errorf("another login turned it off (%q)", why)
		}
		if err := asha.server(func(tx pgx.Tx) error { return auth.TurnOffDevice(ctx, tx, asha.id, mine, "turned_off") }); err != nil {
			t.Fatal(err)
		}
		if why := asha.ended(t, mine); why != "turned_off" {
			t.Errorf("turned off = %q", why)
		}
	})
}

// TestPhoneNotesEnd: logging out ends that session's device only; Log out on all devices, a password
// change and the operator's new password end every device; another login turning notes on (or just
// opening Seyal) in the same browser ends the first login's row; and a device not opened for 30 days
// ends and is never handed to the sender.
func TestPhoneNotesEnd(t *testing.T) {
	ctx := context.Background()
	asha := newLogin(t, "Ending One")

	t.Run("log out ends this browser's device only", func(t *testing.T) {
		phone, laptop := asha.logIn(t), asha.logIn(t)
		p := asha.turnOn(t, phone, newDevice(t))
		l := asha.turnOn(t, laptop, newDevice(t))
		if err := asha.server(func(tx pgx.Tx) error { return auth.LogOut(ctx, tx, phone) }); err != nil {
			t.Fatal(err)
		}
		if asha.ended(t, p) != "logged_out" || asha.ended(t, l) != "" {
			t.Errorf("after logging out on the phone: phone %q, laptop %q; want logged_out and still on", asha.ended(t, p), asha.ended(t, l))
		}
	})

	t.Run("log out on all devices ends every device", func(t *testing.T) {
		a, b := asha.turnOn(t, asha.logIn(t), newDevice(t)), asha.turnOn(t, asha.logIn(t), newDevice(t))
		if err := asha.server(func(tx pgx.Tx) error { return auth.LogOutEverywhere(ctx, tx, asha.id) }); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{a, b} {
			if why := asha.ended(t, id); why != "logged_out_everywhere" {
				t.Errorf("device %s = %q, want logged_out_everywhere", id, why)
			}
		}
	})

	t.Run("a new password ends every device", func(t *testing.T) {
		changed := asha.turnOn(t, asha.logIn(t), newDevice(t))
		next := "a brand new password 1"
		if err := asha.server(func(tx pgx.Tx) error { return auth.ChangePassword(ctx, tx, asha.id, asha.password, next) }); err != nil {
			t.Fatal(err)
		}
		asha.password = next
		reset := asha.turnOn(t, asha.logIn(t), newDevice(t))
		if err := asha.server(func(tx pgx.Tx) error { // the operator's reset (`seyal set-password`)
			var err error
			asha.password, _, err = auth.NewPassword(ctx, tx, asha.email, "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if asha.ended(t, changed) != "password" || asha.ended(t, reset) != "password" {
			t.Errorf("after the change %q, after the reset %q; want password both", asha.ended(t, changed), asha.ended(t, reset))
		}
	})

	t.Run("the same browser for a second login ends the first", func(t *testing.T) {
		ben := newLogin(t, "Ending Shared")
		shared := newDevice(t)
		first := asha.turnOn(t, asha.logIn(t), shared)
		second := ben.turnOn(t, ben.logIn(t), shared)
		if asha.ended(t, first) != "other_login" || ben.ended(t, second) != "" {
			t.Errorf("Ben turned on in Asha's browser: Asha's %q, Ben's %q; want other_login and on", asha.ended(t, first), ben.ended(t, second))
		}
		// Only opening Seyal there is enough: the earlier login's row ends and the new one sees Off.
		again := newDevice(t)
		mine := asha.turnOn(t, asha.logIn(t), again)
		if h := ben.hello(t, ben.logIn(t), again.Endpoint); h.State != auth.DeviceOff || asha.ended(t, mine) != "other_login" {
			t.Errorf("Ben opened Seyal in Asha's browser: Ben sees %+v, Asha's row %q; want off and other_login", h, asha.ended(t, mine))
		}
		token := asha.logIn(t)
		if h := asha.hello(t, token, shared.Endpoint); h.State != auth.DeviceOff {
			t.Errorf("Asha back in a browser Ben has = %+v, want off", h)
		}
	})

	t.Run("a hello moves the device onto the new session", func(t *testing.T) {
		d := newDevice(t)
		id := asha.turnOn(t, asha.logIn(t), d)
		token := asha.logIn(t) // e.g. after the first session simply expired
		if h := asha.hello(t, token, d.Endpoint); h.State != auth.DeviceOn || h.DeviceID != id {
			t.Fatalf("hello = %+v, want on, row %s", h, id)
		}
		if err := asha.server(func(tx pgx.Tx) error { return auth.LogOut(ctx, tx, token) }); err != nil {
			t.Fatal(err)
		}
		if why := asha.ended(t, id); why != "logged_out" {
			t.Errorf("logging out of the new session = %q, want logged_out (the device follows the session)", why)
		}
	})

	t.Run("not opened for 30 days", func(t *testing.T) {
		token := asha.logIn(t)
		quiet, stale := newDevice(t), newDevice(t)
		q := asha.turnOn(t, token, quiet)
		s := asha.turnOn(t, token, stale)
		if _, err := asha.owner.Exec(ctx, `UPDATE push_devices SET last_opened_at = now() - interval '31 days' WHERE id IN ($1, $2)`, q, s); err != nil {
			t.Fatal(err)
		}
		var here []auth.BusinessDevice
		if err := asha.in(asha.biz[0], func(tx pgx.Tx) error {
			var err error
			here, err = auth.BusinessDevices(ctx, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, d := range here {
			if d.DeviceID == q || d.DeviceID == s {
				t.Errorf("the sender would get device %s, not opened for 31 days", d.DeviceID)
			}
		}
		if h := asha.hello(t, token, stale.Endpoint); h.State != auth.DeviceEnded || asha.ended(t, s) != "unused" {
			t.Errorf("opening Seyal after 31 days = %+v (%q), want ended as unused", h, asha.ended(t, s))
		}
		if h := asha.hello(t, token, stale.Endpoint); h.State != auth.DeviceEnded {
			t.Errorf("the next hello = %+v, want still ended (Turn on again)", h)
		}
		asha.devices(t) // Me ends the others
		if why := asha.ended(t, q); why != "unused" {
			t.Errorf("after Me opened, the other old device = %q, want unused", why)
		}
	})
}

// TestDeviceRowsStayWithTheirLogin: a business's transaction, another login's and the server's see no
// device rows; the sender's look-up (push_devices_here) gives only the current business's active
// people's devices, and nothing without a business.
func TestDeviceRowsStayWithTheirLogin(t *testing.T) {
	ctx := context.Background()
	asha := newLogin(t, "Rows One", "Rows Two")
	ben := newLogin(t, "Rows Three")
	mine := asha.turnOn(t, asha.logIn(t), newDevice(t))
	bens := ben.turnOn(t, ben.logIn(t), newDevice(t))

	count := func(what string, run func(func(pgx.Tx) error) error) int {
		t.Helper()
		var n int
		if err := run(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM push_devices`).Scan(&n)
		}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return n
	}
	for _, b := range append(asha.biz, ben.biz...) {
		if n := count("a business", func(fn func(pgx.Tx) error) error { return asha.in(b, fn) }); n != 0 {
			t.Errorf("a transaction of business %s sees %d device rows, want none", b.tenant, n)
		}
	}
	if n := count("the server", asha.server); n != 0 {
		t.Errorf("a server-wide transaction sees %d device rows, want none", n)
	}
	if n := count("Ben's login", func(fn func(pgx.Tx) error) error { return db.WithAccount(ctx, ben.app, ben.id, fn) }); n != 1 {
		t.Errorf("Ben's login sees %d device rows, want his own one", n)
	}
	err := db.WithAccount(ctx, ben.app, ben.id, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE push_devices SET ended_at = now(), ended_why = 'turned_off' WHERE id = $1`, mine)
		return err
	})
	if err != nil || asha.ended(t, mine) != "" {
		t.Errorf("Ben's login changing Asha's row = %v, row %q; want nothing changed", err, asha.ended(t, mine))
	}

	here := func(b biz) []string {
		t.Helper()
		var ids []string
		if err := asha.in(b, func(tx pgx.Tx) error {
			list, err := auth.BusinessDevices(ctx, tx)
			for _, d := range list {
				ids = append(ids, d.DeviceID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	for _, b := range asha.biz { // one device serves every business of the login
		if got := here(b); len(got) != 1 || got[0] != mine {
			t.Errorf("look-up in Asha's business %s = %v, want her device only", b.tenant, got)
		}
	}
	if got := here(ben.biz[0]); len(got) != 1 || got[0] != bens {
		t.Errorf("look-up in Ben's business = %v, want his device only", got)
	}
	var none int
	if err := asha.server(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM push_devices_here()`).Scan(&none)
	}); err != nil || none != 0 {
		t.Errorf("look-up with no business = %d (%v), want nothing", none, err)
	}
	// An iPhone with one Home Screen icon per business: each icon has its own address and its own
	// row, and only its own business sends to it, so a note never shows twice on that phone.
	iconA, err := asha.tryTurnOnAt(asha.logIn(t), newDevice(t), asha.biz[0].tenant)
	if err != nil {
		t.Fatal(err)
	}
	iconB, err := asha.tryTurnOnAt(asha.logIn(t), newDevice(t), asha.biz[1].tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got := here(asha.biz[0]); !slices.Equal(got, []string{mine, iconA}) {
		t.Errorf("look-up in business A with an icon per business = %v, want %v (her browser and A's icon)", got, []string{mine, iconA})
	}
	if got := here(asha.biz[1]); !slices.Equal(got, []string{mine, iconB}) {
		t.Errorf("look-up in business B with an icon per business = %v, want %v (her browser and B's icon)", got, []string{mine, iconB})
	}
	for _, id := range []string{iconA, iconB} {
		if err := asha.server(func(tx pgx.Tx) error {
			return auth.TurnOffDevice(ctx, tx, asha.id, id, "turned_off")
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Switched off in one business: no phone notes from it, still from the other.
	sqlIn(t, asha.owner, asha.biz[0].tenant, `UPDATE persons SET status = 'disabled' WHERE id = $1`, asha.biz[0].person)
	if got := here(asha.biz[0]); len(got) != 0 {
		t.Errorf("look-up where Asha is switched off = %v, want nothing", got)
	}
	if got := here(asha.biz[1]); len(got) != 1 {
		t.Errorf("look-up in Asha's other business = %v, want her device still", got)
	}
}
