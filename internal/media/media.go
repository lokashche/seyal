// SPDX-License-Identifier: AGPL-3.0-or-later

// Package media keeps the bytes of files people send (voice notes, photos, documents). Files are
// stored by their SHA-256, so a file can't be swapped without its fingerprint changing, and the
// same file sent twice is stored once. Nothing is ever deleted.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxSize is the largest file accepted (25 MB): enough for several minutes of voice.
const MaxSize = 25 << 20

// accepted are the file types that may be stored: voice notes, photos and PDFs. SVG images are not
// among them, because an SVG can contain scripts.
var accepted = map[string]bool{
	"audio/webm": true, "audio/ogg": true, "audio/mp4": true, "audio/mpeg": true, "audio/aac": true,
	"audio/wav": true, "audio/x-m4a": true,
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/heic": true,
	"application/pdf": true,
}

// Clean returns the plain type of a file ("audio/webm" from "audio/webm;codecs=opus") if it may be
// stored, and false otherwise. Only the plain type is ever kept and served back: a type as sent by
// a browser (or anyone) can carry tricks such as "image/png,text/html" that make a browser treat
// the file as a web page.
func Clean(contentType string) (string, bool) {
	base, _, err := mime.ParseMediaType(contentType)
	if err != nil || !accepted[base] {
		return "", false
	}
	return base, true
}

// Sniff fixes the type of an Ogg recording that came without a usable one. A chat app's voice note
// (.opus) shared from an Android phone may arrive as "application/octet-stream", or as "audio/opus",
// which isn't on the list (D93). When the file's first bytes are an Ogg file's ("OggS"), its type is
// "audio/ogg", a type Seyal already keeps and measures. Any other type is returned unchanged, so
// Clean still decides.
func Sniff(contentType string, data []byte) string {
	base, _, _ := mime.ParseMediaType(contentType)
	switch base {
	case "", "application/octet-stream", "audio/opus":
		if len(data) >= 4 && string(data[:4]) == "OggS" {
			return "audio/ogg"
		}
	}
	return contentType
}

// Allowed reports whether a file of this type may be stored.
func Allowed(contentType string) bool {
	_, ok := Clean(contentType)
	return ok
}

// Store keeps file bytes.
type Store interface {
	// Put stores data under key. Storing the same key again is fine (the bytes are the same).
	Put(key string, data []byte) error
	// Open returns the bytes stored under key.
	Open(key string) (io.ReadCloser, error)
}

var (
	hexPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Key returns the storage key for a tenant's file with the given SHA-256 (hex), e.g.
// "<tenant>/ab/abcdef...". Keys are built only from these two checked values, so they can't
// point outside the store.
func Key(tenantID, sha256Hex string) (string, error) {
	if !uuidPattern.MatchString(tenantID) || !hexPattern.MatchString(sha256Hex) {
		return "", errors.New("media: bad tenant id or fingerprint")
	}
	return tenantID + "/" + sha256Hex[:2] + "/" + sha256Hex, nil
}

// Fingerprint returns the SHA-256 of data in hex.
func Fingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Disk is a Store in a folder on this computer.
type Disk struct {
	Dir string
}

func (d Disk) path(key string) (string, error) {
	tenant, rest, ok := strings.Cut(key, "/")
	prefix, name, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || !uuidPattern.MatchString(tenant) || !hexPattern.MatchString(name) || prefix != name[:2] {
		return "", fmt.Errorf("media: bad key %q", key)
	}
	return filepath.Join(d.Dir, tenant, prefix, name), nil
}

// Put writes the file to a temporary name first and then renames it, so a crash never leaves a
// half-written file under the real name.
func (d Disk) Put(key string, data []byte) error {
	path, err := d.path(key)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // already stored; same key means same bytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (d Disk) Open(key string) (io.ReadCloser, error) {
	path, err := d.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}
