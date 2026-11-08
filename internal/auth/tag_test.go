// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"strings"
	"testing"
	"time"
)

// TestTag: a login's tag is the same in all its sessions and businesses, differs between logins, and
// doesn't show the login's id (D91: a share record waits on the device under it).
func TestTag(t *testing.T) {
	const account = "0b7e5f4e-1c2d-4a8b-9e3f-000000000001"
	a := Session{AccountID: account, PersonID: "p1", ExpiresAt: time.Unix(1000, 0)}
	b := Session{AccountID: account, PersonID: "p2", ExpiresAt: time.Unix(2000, 0)}
	if a.Tag() == "" || a.Tag() != b.Tag() {
		t.Errorf("one login's tags differ: %q, %q", a.Tag(), b.Tag())
	}
	if other := (Session{AccountID: "0b7e5f4e-1c2d-4a8b-9e3f-000000000002"}); other.Tag() == a.Tag() {
		t.Error("two logins have the same tag")
	}
	if strings.Contains(a.Tag(), "0b7e5f4e") {
		t.Error("the tag shows the login's id")
	}
	if (Session{}).Tag() != "" {
		t.Error("no login has a tag")
	}
}
