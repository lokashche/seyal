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
		"audio/webm;codecs=opus":  true,
		"audio/mp4":               true,
		"image/jpeg":              true,
		"application/pdf":         true,
		"text/html":               false,
		"image/svg+xml":           false, // can contain scripts
		"application/x-msdos":     false,
		"image/png,text/html":     false, // a browser would take the last type and show a web page
		"audio/x,text/javascript": false,
		"image/x-anything":        false,
		"":                        false,
	} {
		if got := Allowed(typ); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", typ, got, want)
		}
	}
}

// TestSniffOggVoiceNote: a WhatsApp voice note shared from Android may come as
// application/octet-stream or audio/opus; its first bytes (OggS) make it audio/ogg (D93). Nothing
// else changes type by its bytes.
func TestSniffOggVoiceNote(t *testing.T) {
	ogg := []byte("OggS\x00\x02pretend opus")
	for _, tt := range []struct {
		typ  string
		data []byte
		want string
	}{
		{"application/octet-stream", ogg, "audio/ogg"},
		{"audio/opus", ogg, "audio/ogg"},
		{"", ogg, "audio/ogg"},
		{"application/octet-stream", []byte("%PDF-1.7"), "application/octet-stream"}, // not Ogg: still refused
		{"text/html", ogg, "text/html"},                                              // a stated type is never overruled
		{"image/png", ogg, "image/png"},                                              // nor a kept one
		{"application/octet-stream", []byte("Og"), "application/octet-stream"},
	} {
		if got := Sniff(tt.typ, tt.data); got != tt.want {
			t.Errorf("Sniff(%q, %q) = %q, want %q", tt.typ, tt.data, got, tt.want)
		}
	}
	if !Allowed(Sniff("application/octet-stream", ogg)) {
		t.Error("a sniffed voice note isn't allowed")
	}
}
