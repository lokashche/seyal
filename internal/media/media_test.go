// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"io"
	"testing"
)

const tenant = "11111111-2222-3333-4444-555555555555"

func TestDiskStore(t *testing.T) {
	store := Disk{Dir: t.TempDir()}
	data := []byte("pretend this is a voice note")
	key, err := Key(tenant, Fingerprint(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(key, data); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(key, data); err != nil {
		t.Fatalf("storing the same file twice: %v", err)
	}
	r, err := store.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != string(data) {
		t.Errorf("read back %q", got)
	}
}

func TestKeysCannotEscapeTheStore(t *testing.T) {
	store := Disk{Dir: t.TempDir()}
	for _, key := range []string{
		"../../etc/passwd",
		tenant + "/../x",
		tenant + "/ab/" + "../../../../secret",
		tenant + "/zz/" + Fingerprint([]byte("x")), // prefix must match the fingerprint
	} {
		if err := store.Put(key, []byte("x")); err == nil {
			t.Errorf("Put accepted key %q", key)
		}
	}
	if _, err := Key("not-a-tenant", Fingerprint([]byte("x"))); err == nil {
		t.Error("Key accepted a bad tenant id")
	}
}

func TestAllowed(t *testing.T) {
	for typ, want := range map[string]bool{
		"audio/webm;codecs=opus": true,
		"audio/mp4":              true,
		"image/jpeg":             true,
		"application/pdf":        true,
		"text/html":              false,
		"image/svg+xml":          false, // can contain scripts
		"application/x-msdos":    false,
		"":                       false,
	} {
		if got := Allowed(typ); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", typ, got, want)
		}
	}
}
