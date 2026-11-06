// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

// Phone notes (Phase 4, part 4a-2, D87): the browsers a login turned phone notes on in (push_devices,
// migration 00024). A device belongs to a login, not to one business: one browser serves every
// business of the login (D64). Row-level security shows a transaction only its own login's devices
// (app.account_id), so these functions switch tx to the login's scope for their work and then put the
// scope back, the way Businesses does. Phone notes are not work, so nothing here is a ledger event.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/push"
)

const (
	// MaxDevices is how many browsers one login may have phone notes on at once. A new one past it
	// ends the oldest.
	MaxDevices = 5
	// UnusedDays is how long a device keeps getting phone notes after Seyal was last opened in it.
	// After that its row ends ("unused"): a phone in a drawer, or handed on without a reset, stops
	// showing item numbers and names on its lock screen.
	UnusedDays = 30
)

// ErrBadDevice means the browser's device push address or lock keys are not usable (not on a push
// service, not a real key). It never holds the address or a key.
var ErrBadDevice = errors.New("auth: this browser's phone-note details are not usable")

// Device is one browser with phone notes on, as Me shows it. It never holds the device push address
// or the lock keys: those never appear on a page.
type Device struct {
	ID           string
	Label        string // e.g. "Chrome on Android"
	CreatedAt    time.Time
	LastOpenedAt time.Time
}

// Device states for the browser asking (Hello).
const (
	DeviceOn    = "on"    // phone notes are on for this browser
	DeviceOff   = "off"   // they are not (never turned on, turned off, or another login has it)
	DeviceEnded = "ended" // Seyal ended them: unused for 30 days, or the push service said it is gone
)

// Hello is what the server knows about the browser that asks.
type Hello struct {
	State    string // DeviceOn, DeviceOff or DeviceEnded
	DeviceID string // when on
	Label    string // when on
}

// asLogin runs fn with tx in the login's own scope (it sees only the login's devices and people), then
// puts the scope back. Any transaction will do.
func asLogin(ctx context.Context, tx pgx.Tx, accountID string, fn func() error) error {
	if accountID == "" {
		return errors.New("auth: empty login id")
	}
	was, err := db.CurrentScope(ctx, tx)
	if err != nil {
		return err
	}
	if err := db.SetScope(ctx, tx, db.Scope{Account: accountID}); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return db.SetScope(ctx, tx, was)
}

// openSession returns the login of a token's open session and the session's key, or ErrNoSession.
func openSession(ctx context.Context, tx pgx.Tx, token string) (accountID, session string, err error) {
	if token == "" {
		return "", "", ErrNoSession
	}
	session = fingerprint(token)
	err = tx.QueryRow(ctx, `
		SELECT account_id::text FROM account_sessions
		WHERE token_sha256 = $1 AND ended_at IS NULL AND expires_at > now()`, session).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoSession
	}
	return accountID, session, err
}

// TurnOnDevice saves the browser of the login session token as a device with phone notes on, and
// returns its id. origin is the address the browser used (https://host), for the links in its phone
// notes; label says what it is ("Chrome on Android"). The same browser again keeps its row, now on
// this session. If the browser's address is still open for another login (a shared phone), that
// login's row ends. At most MaxDevices stay open for the login: a new one past it ends the oldest.
// homeTenant is set only for an iPhone Home Screen icon, which is one business's own app with its own
// address: then only that business sends to it (push_devices_here). "" serves every business of the
// login. Any transaction will do.
func TurnOnDevice(ctx context.Context, tx pgx.Tx, token string, d push.Device, origin, label, homeTenant string) (string, error) {
	d.P256dh, d.Auth = strings.TrimRight(d.P256dh, "="), strings.TrimRight(d.Auth, "=")
	if err := d.Check(); err != nil {
		return "", ErrBadDevice
	}
	accountID, session, err := openSession(ctx, tx, token)
	if err != nil {
		return "", err
	}
	var id string
	err = asLogin(ctx, tx, accountID, func() error {
		if _, err := tx.Exec(ctx, `SELECT push_device_take($1)`, d.Endpoint); err != nil {
			return fmt.Errorf("auth: take device: %w", err)
		}
		// The same browser with the same keys keeps its row; new keys mean a new subscription.
		var p256dh, auth string
		err := tx.QueryRow(ctx, `
			SELECT id::text, p256dh, auth FROM push_devices WHERE endpoint = $1 AND ended_at IS NULL FOR UPDATE`,
			d.Endpoint).Scan(&id, &p256dh, &auth)
		switch {
		case err == nil && p256dh == d.P256dh && auth == d.Auth:
			_, err = tx.Exec(ctx, `
				UPDATE push_devices SET account_session_id = $2, last_opened_at = now() WHERE id = $1`, id, session)
			return err
		case err == nil:
			if err := endDevice(ctx, tx, id, "replaced"); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO push_devices (account_id, account_session_id, endpoint, p256dh, auth, origin, label, home_tenant)
			VALUES (current_account(), $1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid) RETURNING id::text`,
			session, d.Endpoint, d.P256dh, d.Auth, origin, cleanLabel(label), homeTenant).Scan(&id); err != nil {
			return fmt.Errorf("auth: save device: %w", err)
		}
		// The oldest past MaxDevices end.
		_, err = tx.Exec(ctx, `
			UPDATE push_devices SET ended_at = now(), ended_why = 'too_many'
			WHERE id IN (SELECT id FROM push_devices WHERE ended_at IS NULL
				ORDER BY created_at DESC, id OFFSET $1)`, MaxDevices)
		return err
	})
	return id, err
}

// HelloDevice is the browser of the login session token telling Seyal it was opened, with its device
// push address. A row of this login with that address moves onto this session and counts as opened
// now, unless Seyal wasn't opened in it for UnusedDays (then it ends, "unused"). A row of another
// login with that address ends (a shared phone: the new person sees Off). Any transaction will do.
func HelloDevice(ctx context.Context, tx pgx.Tx, token, endpoint string) (Hello, error) {
	accountID, session, err := openSession(ctx, tx, token)
	if err != nil {
		return Hello{}, err
	}
	if push.CheckEndpoint(endpoint) != nil {
		return Hello{State: DeviceOff}, nil
	}
	h := Hello{State: DeviceOff}
	err = asLogin(ctx, tx, accountID, func() error {
		if _, err := tx.Exec(ctx, `SELECT push_device_take($1)`, endpoint); err != nil {
			return fmt.Errorf("auth: take device: %w", err)
		}
		var unused bool
		err := tx.QueryRow(ctx, `
			SELECT id::text, label, last_opened_at <= now() - make_interval(days => $2) FROM push_devices
			WHERE endpoint = $1 AND ended_at IS NULL FOR UPDATE`, endpoint, UnusedDays).Scan(&h.DeviceID, &h.Label, &unused)
		switch {
		case err == nil && unused:
			id := h.DeviceID
			h = Hello{State: DeviceEnded}
			return endDevice(ctx, tx, id, "unused")
		case err == nil:
			h.State = DeviceOn
			_, err = tx.Exec(ctx, `
				UPDATE push_devices SET account_session_id = $2, last_opened_at = now() WHERE id = $1`, h.DeviceID, session)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// Not open: was it Seyal that ended it, the last time?
		var why string
		err = tx.QueryRow(ctx, `
			SELECT ended_why FROM push_devices WHERE endpoint = $1 ORDER BY ended_at DESC LIMIT 1`, endpoint).Scan(&why)
		if err == nil && (why == "unused" || why == "gone") {
			h.State = DeviceEnded
		}
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
		}
		return err
	})
	return h, err
}

// TurnOffDevice ends one of the login's devices: "turned_off" when the person pressed Turn off, or
// "blocked" when the browser's permission was taken back. Another login's device can't be ended
// here. Ending one that has ended already changes nothing. Any transaction will do.
func TurnOffDevice(ctx context.Context, tx pgx.Tx, accountID, deviceID, why string) error {
	if why != "turned_off" && why != "blocked" {
		return errors.New("auth: a device is turned off or blocked")
	}
	return asLogin(ctx, tx, accountID, func() error {
		return endDevice(ctx, tx, deviceID, why)
	})
}

// Devices lists the login's devices with phone notes on, oldest first. Devices not opened for
// UnusedDays end first, so the list shows only those that still get phone notes. Any transaction
// will do.
func Devices(ctx context.Context, tx pgx.Tx, accountID string) ([]Device, error) {
	var list []Device
	err := asLogin(ctx, tx, accountID, func() error {
		if _, err := tx.Exec(ctx, `
			UPDATE push_devices SET ended_at = now(), ended_why = 'unused'
			WHERE ended_at IS NULL AND last_opened_at <= now() - make_interval(days => $1)`, UnusedDays); err != nil {
			return fmt.Errorf("auth: end unused devices: %w", err)
		}
		rows, _ := tx.Query(ctx, `
			SELECT id::text, label, created_at, last_opened_at FROM push_devices
			WHERE ended_at IS NULL ORDER BY created_at, id`)
		var err error
		list, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Device])
		return err
	})
	return list, err
}

// BusinessDevice is a device of an active person of the current business, for sending a phone note.
type BusinessDevice struct {
	PersonID string
	DeviceID string
	Device   push.Device
	Origin   string // the address the browser used, for the link
}

// BusinessDevices returns the devices of the current business's active people that still get phone
// notes (open, and opened in the last UnusedDays). It reads them through push_devices_here(), the
// one narrow way to other logins' lock keys (migration 00024). tx must come from db.WithTenant.
func BusinessDevices(ctx context.Context, tx pgx.Tx) ([]BusinessDevice, error) {
	rows, _ := tx.Query(ctx, `
		SELECT person_id::text, device_id::text, endpoint, p256dh, auth, origin FROM push_devices_here()`)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (BusinessDevice, error) {
		var d BusinessDevice
		err := r.Scan(&d.PersonID, &d.DeviceID, &d.Device.Endpoint, &d.Device.P256dh, &d.Device.Auth, &d.Origin)
		return d, err
	})
}

// endDevice ends an open device of the current login with why.
func endDevice(ctx context.Context, tx pgx.Tx, deviceID, why string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE push_devices SET ended_at = now(), ended_why = $2 WHERE id::text = $1 AND ended_at IS NULL`,
		deviceID, why); err != nil {
		return fmt.Errorf("auth: end device: %w", err)
	}
	return nil
}

// endDevices ends every open device of a login (why: "logged_out_everywhere" or "password").
func endDevices(ctx context.Context, tx pgx.Tx, accountID, why string) error {
	return asLogin(ctx, tx, accountID, func() error {
		if _, err := tx.Exec(ctx, `
			UPDATE push_devices SET ended_at = now(), ended_why = $1 WHERE ended_at IS NULL`, why); err != nil {
			return fmt.Errorf("auth: end devices: %w", err)
		}
		return nil
	})
}

// endSessionDevices ends the open devices of one login session (logging out in that browser).
func endSessionDevices(ctx context.Context, tx pgx.Tx, accountID, session string) error {
	return asLogin(ctx, tx, accountID, func() error {
		if _, err := tx.Exec(ctx, `
			UPDATE push_devices SET ended_at = now(), ended_why = 'logged_out'
			WHERE account_session_id = $1 AND ended_at IS NULL`, session); err != nil {
			return fmt.Errorf("auth: end devices: %w", err)
		}
		return nil
	})
}

// cleanLabel keeps a device's label short and valid text.
func cleanLabel(label string) string {
	label = strings.ToValidUTF8(strings.TrimSpace(label), "")
	if r := []rune(label); len(r) > 60 {
		label = string(r[:60])
	}
	return label
}
