// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// Other apps (D95): each business lists, in its private set-up file, the web addresses Seyal may send
// to ("targets"). `seyal import` writes them to the other_apps table with the owner connection; the
// app only reads them. An admin's Pause, and Seyal's own pause, are kept apart in other_app_state, so an
// import never undoes them.

// What a target's reply must say for a send to count as done.
const (
	ExpectOKJSON = "ok_json" // JSON with "ok": true (an Apps Script web app answers 200 even when it fails)
	Expect2xx    = "2xx"     // any 2xx reply code
)

// Target is one other app a business may send to: a row of other_apps.
type Target struct {
	Name       string   // the key a process names it by ("sheet-one")
	Label      string   // shown to people ("Sheet one")
	Service    string   // who runs it, shown in "Leaves Seyal: these values, to <service>"
	URL        string   // secret: never shown, logged, exported or put in the ledger
	RedirectTo []string // hosts one redirect may go to
	Expect     string   // ExpectOKJSON or Expect2xx
	Fields     []string // the most fields it may ever receive
	Link       bool     // may a process add the item's link
	Secret     string   // the signing secret ("whsec_…"): secret like URL
	Paused     bool     // paused in the set-up file
}

// Host is the target's host, in lower case, without a port ("" for an address that isn't one).
func (t Target) Host() string {
	u, err := url.Parse(t.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// AddressEnd is the most of the address ever shown: its end, but only when that end is a fixed word
// that is the same in every address of its kind, like Apps Script's "…/exec". Other services (Zapier,
// Make, n8n) put the random code, which works like a password, at the end, so for them it is "" and
// only the host is shown. The ledger records the same value (in Summary), so it never holds part of a
// code.
func (t Target) AddressEnd() string {
	u, err := url.Parse(t.URL)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	for _, word := range fixedEnds {
		if strings.HasSuffix(u.Path, "/"+word) {
			return "…/" + word
		}
	}
	return ""
}

// fixedEnds are last parts of an address that never carry a code: an Apps Script web app's address
// ends in /exec (or /dev while it is being tried out).
var fixedEnds = []string{"exec", "dev"}

// HostsOff lists the target's hosts that hosts doesn't allow: its own host first, then each host its
// answer may redirect to. A redirect to a host not allowed isn't followed, so a send that reached the
// other app (an Apps Script runs, then answers with a redirect to script.googleusercontent.com) would
// look like a failure. Empty means every host is allowed.
func (t Target) HostsOff(hosts Hosts) []string {
	var off []string
	for _, h := range append([]string{t.Host()}, t.RedirectTo...) {
		if h = strings.ToLower(strings.TrimSpace(h)); !hosts.Allows(h) && !slices.Contains(off, h) {
			off = append(off, h)
		}
	}
	return off
}

// Hash is a SHA-256 of every column of the target, in hex. The ledger keeps it (never the address or
// the secret), so `seyal verify` finds a row changed outside Seyal.
func (t Target) Hash() string {
	b, _ := json.Marshal([]any{t.Name, t.Label, t.Service, t.URL, nonNil(t.RedirectTo), t.Expect,
		nonNil(t.Fields), t.Link, t.Secret, t.Paused})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Summary is what the ledger records about a target: everything but the address and the secret,
// with secret saying what happened to the secret ("new", "kept" or "replaced").
func (t Target) Summary(secret string) map[string]any {
	return map[string]any{"label": t.Label, "service": t.Service, "host": t.Host(), "address_ends": t.AddressEnd(),
		"redirect_to": nonNil(t.RedirectTo), "expect": t.Expect, "fields": nonNil(t.Fields), "link": t.Link,
		"paused": t.Paused, "secret": secret, "sha256": t.Hash()}
}

// ChangeKey is the key of a target's change in a business.updated event: "other_app:<name>".
const ChangeKey = "other_app:"

// pausedKey is the key of an admin's Pause or Resume in a business.updated event.
const pausedKey = "other_app_paused:"

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

var (
	namePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	fieldPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
)

// DefaultExpect is what a target on host expects when the set-up file doesn't say: Apps Script web
// apps answer JSON with "ok", others any 2xx.
func DefaultExpect(host string) string {
	if host == scriptHost {
		return ExpectOKJSON
	}
	return Expect2xx
}

// Check reports every problem with a target as written in a set-up file. The messages never hold
// the address or the secret. The host list of the server is not checked here: a target may be set up
// before its host is allowed (it waits until then).
func (t Target) Check() []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("other app %q: ", t.Name)+fmt.Sprintf(format, args...))
	}
	if !namePattern.MatchString(t.Name) {
		add("a name is lowercase letters, digits and -, at most 63")
	}
	if l := strings.TrimSpace(t.Label); l == "" || utf8.RuneCountInString(l) > 100 {
		add("label is required (at most 100 characters)")
	}
	if s := strings.TrimSpace(t.Service); s == "" || utf8.RuneCountInString(s) > 60 {
		add("service is required (who runs it, for example Google; at most 60 characters)")
	}
	if _, err := CheckAddress(t.URL, func(string) bool { return true }); err != nil {
		add("url: %s (write the full https:// address the other app gave, with no port)", err)
	} else if _, err := ParseHosts(t.Host()); err != nil {
		add("url: the host must be a name like script.google.com, not an IP address")
	}
	for _, h := range t.RedirectTo {
		if _, err := ParseHosts(h); err != nil || strings.Contains(h, ",") || strings.TrimSpace(h) == "" {
			add("redirect_to: write host names only, like script.googleusercontent.com")
			break
		}
	}
	if t.Expect != "" && t.Expect != ExpectOKJSON && t.Expect != Expect2xx {
		add("expect %q: write ok_json or 2xx, or leave it out", t.Expect)
	}
	seen := map[string]bool{}
	for _, f := range t.Fields {
		if !fieldPattern.MatchString(f) || seen[f] {
			add("fields: %q is not a field key, or is listed twice", f)
		}
		seen[f] = true
	}
	return problems
}

// Clean returns the target as it is stored: words trimmed, "https://" and host names in lower case,
// and the expectation filled in.
func (t Target) Clean() Target {
	t.Label, t.Service, t.URL = strings.TrimSpace(t.Label), strings.TrimSpace(t.Service), strings.TrimSpace(t.URL)
	if p := len("https://"); len(t.URL) >= p && strings.EqualFold(t.URL[:p], "https://") {
		t.URL = "https://" + t.URL[p:] // "HTTPS://" is the same address; the table's check wants lower case
	}
	hosts := []string{}
	for _, h := range t.RedirectTo {
		hosts = append(hosts, strings.ToLower(strings.TrimSpace(h)))
	}
	t.RedirectTo, t.Fields = hosts, nonNil(t.Fields)
	if t.Expect == "" {
		t.Expect = DefaultExpect(t.Host())
	}
	return t
}

// State is what Seyal and the admins changed about a target in the app: a pause (why: "admin",
// "gone" or "cap"), and the last test.
type State struct {
	PausedAt   time.Time // zero: not paused in the app
	PausedBy   string    // the admin's person id, for why "admin"
	Why        string
	TestedAt   time.Time
	TestedBy   string
	TestResult string // one of the fixed short reasons
}

// Paused reports whether the app paused it.
func (s State) Paused() bool { return !s.PausedAt.IsZero() }

// Listed is a target with its state.
type Listed struct {
	Target
	State State
}

// Targets reads the current business's targets, by name. tx must come from db.WithTenant.
func Targets(ctx context.Context, tx pgx.Tx) ([]Listed, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.name, a.label, a.service, a.url, a.redirect_to, a.expect, a.fields, a.link, a.secret, a.paused,
			s.paused_at, coalesce(s.paused_by::text, ''), coalesce(s.why, ''),
			s.tested_at, coalesce(s.tested_by::text, ''), coalesce(s.test_result, '')
		FROM other_apps a LEFT JOIN other_app_state s ON s.tenant_id = a.tenant_id AND s.name = a.name
		WHERE a.tenant_id = current_tenant() ORDER BY a.name`)
	if err != nil {
		return nil, fmt.Errorf("send: targets: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Listed, error) {
		var l Listed
		var pausedAt, testedAt *time.Time
		err := r.Scan(&l.Name, &l.Label, &l.Service, &l.URL, &l.RedirectTo, &l.Expect, &l.Fields, &l.Link, &l.Secret,
			&l.Paused, &pausedAt, &l.State.PausedBy, &l.State.Why, &testedAt, &l.State.TestedBy, &l.State.TestResult)
		if pausedAt != nil {
			l.State.PausedAt = *pausedAt
		}
		if testedAt != nil {
			l.State.TestedAt = *testedAt
		}
		return l, err
	})
}

// ErrNoTarget is a name the business's set-up file doesn't list.
var ErrNoTarget = errors.New("send: no other app by that name")

// Find returns one of the current business's targets. tx must come from db.WithTenant.
func Find(ctx context.Context, tx pgx.Tx, name string) (Listed, error) {
	all, err := Targets(ctx, tx)
	if err != nil {
		return Listed{}, err
	}
	for _, l := range all {
		if l.Name == name {
			return l, nil
		}
	}
	return Listed{}, ErrNoTarget
}

// Pause pauses a target (paused true) or resumes it, as the admin by (Settings → Processes → Leaves
// Seyal, design 2.4). It is a business.updated ledger event by the admin, written in the same
// transaction. Resume also ends a pause Seyal made (an address switched off, too many sends). The set-up
// file's own paused: true is separate and only the file undoes it. It reports whether anything
// changed. tx must come from db.WithTenant.
func Pause(ctx context.Context, tx pgx.Tx, name string, paused bool, by string) (bool, error) {
	if by == "" {
		return false, errors.New("send: pause: nobody pressed it")
	}
	if err := ledger.Lock(ctx, tx); err != nil {
		return false, err
	}
	t, err := Find(ctx, tx, name)
	if err != nil {
		return false, err
	}
	if t.State.Paused() == paused {
		return false, nil
	}
	var tenantID string
	if err := tx.QueryRow(ctx, `SELECT current_tenant()::text`).Scan(&tenantID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO other_app_state (tenant_id, name, paused_at, paused_by, why)
		VALUES (current_tenant(), $1, CASE WHEN $2 THEN now() END, CASE WHEN $2 THEN $3::uuid END, CASE WHEN $2 THEN 'admin' END)
		ON CONFLICT (tenant_id, name) DO UPDATE SET paused_at = excluded.paused_at, paused_by = excluded.paused_by,
			why = excluded.why`, name, paused, by); err != nil {
		return false, fmt.Errorf("send: pause: %w", err)
	}
	change := map[string]any{"old": t.State.Paused(), "new": paused}
	if !paused {
		change["was_why"] = t.State.Why // a Resume says whose pause it ended
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{Kind: "business.updated", SubjectType: "business", SubjectID: tenantID,
		Payload: map[string]any{"changes": map[string]any{pausedKey + name: change}}}.By(ledger.PersonActor(by)))
	return err == nil, err
}

// Seyal's own reasons to pause a target (D96): its address was switched off (410), or it got more
// than CapPerHour sends in an hour.
const (
	WhyGone = "gone"
	WhyCap  = "cap"
)

// CapPerHour is the most sends one target gets in an hour (design 2.3, step 6).
const CapPerHour = 30

// SeyalPause pauses a target for Seyal's own reason why (WhyGone or WhyCap), unless it is already
// paused in the app. Like an admin's Pause, it is a business.updated ledger event (by Seyal), written in
// the same transaction; an admin's Resume ends it. It reports whether anything changed. The caller holds
// the ledger head lock (ledger.Lock). tx must come from db.WithTenant.
func SeyalPause(ctx context.Context, tx pgx.Tx, name, why string) (bool, error) {
	if why != WhyGone && why != WhyCap {
		return false, errors.New("send: pause: unknown reason")
	}
	t, err := Find(ctx, tx, name)
	if err != nil || t.State.Paused() {
		return false, err
	}
	var tenantID string
	if err := tx.QueryRow(ctx, `SELECT current_tenant()::text`).Scan(&tenantID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO other_app_state (tenant_id, name, paused_at, why) VALUES (current_tenant(), $1, now(), $2)
		ON CONFLICT (tenant_id, name) DO UPDATE SET paused_at = excluded.paused_at, paused_by = NULL, why = excluded.why`,
		name, why); err != nil {
		return false, fmt.Errorf("send: pause: %w", err)
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{Kind: "business.updated", SubjectType: "business", SubjectID: tenantID,
		Payload: map[string]any{"changes": map[string]any{pausedKey + name: map[string]any{"old": false, "new": true, "why": why}}}}.
		By(ledger.SystemActor))
	return err == nil, err
}

// Poster sends one post: *Caller is one, and tests use a fake.
type Poster interface {
	PostRedirect(ctx context.Context, address string, redirectTo []string, header http.Header, body []byte) (Answer, error)
}

// The test send's fictional data (design 2.3): the sample sheet script puts rows whose number starts
// with TEST- on a tab of their own.
const (
	TestNumber = "TEST-0000"
	TestName   = "Acme Traders"
)

// TestBody is the body of a test send, in the shape real sends have, with fictional values: the
// number TEST-0000, every listed field "test", and a row TEST-0000, Acme Traders, 0, the currency.
func TestBody(t Target, business, currency, id string, at time.Time) ([]byte, error) {
	fields := map[string]any{}
	for _, f := range t.Fields {
		fields[f] = "test"
	}
	body := map[string]any{
		"type": "seyal.test", "id": id, "timestamp": at.UTC().Format(time.RFC3339), "business": business, "test": true,
		"data": map[string]any{"number": TestNumber, "process": "test", "state": "test", "fields": fields,
			"row": []any{TestNumber, TestName, 0, currency, "test", id}},
	}
	guarded, err := GuardAll(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(guarded)
}

// TestID is a test send's id: never the same twice, so a sheet keeps each test.
func TestID(business string, at time.Time) string {
	return "snd_" + business + "_test_" + strconv.FormatInt(at.UnixNano(), 36)
}

// Test sends body (from TestBody) to the target, signed with its secret, and judges the answer. It
// makes a network call, so it must run outside any transaction.
func Test(ctx context.Context, p Poster, t Target, id string, at time.Time, body []byte) Verdict {
	header, out, err := Signed(t.Secret, id, at, body, SigInBody(t.Host()))
	if err != nil {
		return Verdict{Fix, ReasonFailed} // a broken secret: only an import fixes it
	}
	a, err := p.PostRedirect(ctx, t.URL, t.RedirectTo, header, out)
	return Judge(a, err, t.Expect == ExpectOKJSON)
}

// ErrChanged is a test whose target an import changed or removed while the test was on its way: its
// result is about a set-up that is no longer there, so it is not kept.
var ErrChanged = errors.New("send: the other app's set-up changed during the test")

// RecordTest keeps the last test's time, who pressed it and its fixed short reason, but only while
// the target is still the one tested: tested is the Hash of the target the test was sent to. If an
// import changed or removed it meanwhile (a new secret, say), nothing is kept and it returns
// ErrChanged. It first takes the ledger head lock, which an import also holds while it changes other
// apps, so an import can't change the row between the check and the write. (The app role may only read
// other_apps, so it can't lock the row itself.) It is not a ledger event: a test carries only
// fictional values and changes nothing (D95). tx must come from db.WithTenant.
func RecordTest(ctx context.Context, tx pgx.Tx, name, tested, by, reason string) error {
	if err := ledger.Lock(ctx, tx); err != nil {
		return err
	}
	var now Target
	err := tx.QueryRow(ctx, `
		SELECT name, label, service, url, redirect_to, expect, fields, link, secret, paused
		FROM other_apps WHERE tenant_id = current_tenant() AND name = $1`, name).Scan(
		&now.Name, &now.Label, &now.Service, &now.URL, &now.RedirectTo, &now.Expect, &now.Fields, &now.Link,
		&now.Secret, &now.Paused)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrChanged
	case err != nil:
		return fmt.Errorf("send: keep the test: %w", err)
	case now.Hash() != tested:
		return ErrChanged
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO other_app_state (tenant_id, name, tested_at, tested_by, test_result)
		VALUES (current_tenant(), $1, now(), $2::uuid, $3)
		ON CONFLICT (tenant_id, name) DO UPDATE SET tested_at = excluded.tested_at, tested_by = excluded.tested_by,
			test_result = excluded.test_result`, name, by, reason); err != nil {
		return fmt.Errorf("send: keep the test: %w", err)
	}
	return nil
}

// ForgetTest clears the last test of a target whose address or secret changed, or that left the
// set-up file: that test reached an address or used a secret that is no longer there. A pause stays (an
// import never undoes one). tx must come from db.WithTenant.
func ForgetTest(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE other_app_state SET tested_at = NULL, tested_by = NULL, test_result = NULL
		WHERE tenant_id = current_tenant() AND name = $1 AND tested_at IS NOT NULL`, name); err != nil {
		return fmt.Errorf("send: forget the test: %w", err)
	}
	return nil
}

// RecordedHash is the SHA-256 the ledger last recorded for the target name (from the newest
// business.updated change "other_app:<name>"), and "" when the ledger has none or says it was removed.
// tx must come from db.WithTenant.
func RecordedHash(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	key := ChangeKey + name
	var sum *string
	err := tx.QueryRow(ctx, `
		SELECT body::jsonb -> 'payload' -> 'changes' -> $1 -> 'new' ->> 'sha256'
		FROM ledger WHERE tenant_id = current_tenant() AND kind = 'business.updated'
			AND jsonb_exists(body::jsonb -> 'payload' -> 'changes', $1)
		ORDER BY seq DESC LIMIT 1`, key).Scan(&sum)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("send: the ledger's record of %s: %w", name, err)
	case sum == nil:
		return "", nil
	}
	return *sum, nil
}
