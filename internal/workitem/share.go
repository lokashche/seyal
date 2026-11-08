// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
)

// Sharing out (Phase 4, part 4b-1, D91). The Work Item's Share panel offers ready messages: Seyal's own
// "Tell a colleague" on every item, then the process's own (playbook.ShareMessage). The person checks
// the words, may change them, and hands them to an app on their own device (an email, the phone's share
// menu, the clipboard). Seyal sends nothing. Afterwards the browser reports the hand-over, and Seyal
// records it as an item.shared ledger event, with the exact words and the item's unchanged snapshot:
// no new version, so approvals stay, and the New mark and phone notes don't react to it.

// The ways a share can leave, as the person's browser reports them. The Go code names them only by
// these keys; the page (web/templates) names the apps.
const (
	RouteChatLink = "chat_link" // a chat app's link (part 4b-2)
	RouteEmail    = "email"     // a mailto: link
	RouteCopy     = "copy"      // the clipboard
	RouteMenu     = "menu"      // the phone's (or computer's) own share menu
)

var shareRoutes = []string{RouteChatLink, RouteEmail, RouteCopy, RouteMenu}

// MaxShareText is the most letters the Share box takes, and a share record may hold.
const MaxShareText = 4000

// ColleagueLabel names Seyal's own message (playbook.BuiltinShare).
const ColleagueLabel = "Tell a colleague"

// ErrShareRefused means the person may not hand on that message from this item now: it doesn't exist
// in the item's process version, isn't offered in this step, or is for outside and the person may not
// change the item (or it is closed).
var ErrShareRefused = errors.New("you can't share this message from this item now")

// ShareInputError is a share record that can't be right: an unknown way, a bad attempt id, an empty
// or too long text.
type ShareInputError struct{ Reason string }

func (e *ShareInputError) Error() string { return "share: " + e.Reason }

// ShareEnv is what a share text needs from outside the database.
type ShareEnv struct {
	Link     string // the item's full address in Seyal, for {link}
	Business string // the business's name, for {business}
	Currency string // the business's currency, for money fields
}

// ShareText is one message the panel offers, filled in for this item.
type ShareText struct {
	ID, Label string
	To        string   // playbook.ShareColleague or playbook.ShareOutside
	Text      string   // the ready words
	Empty     []string // the details still empty, shown in the text as "[Label: empty]"
	// Phone and Email are the contact details of the list entry the message names (its contact:), for
	// the chat link and Email buttons (part 4b-2, D92): the phone as digits only (ChatDigits), the email
	// only when it is a plain address (PlainEmail). "" when there is none: the button then asks whom to
	// send to.
	Phone, Email string
}

// ShareRecord is a hand-over as the browser reports it.
type ShareRecord struct {
	ID      string // the message (playbook.BuiltinShare or a process's own)
	Route   string // RouteChatLink, RouteEmail, RouteCopy or RouteMenu
	Text    string // the exact words handed over, after the person's edits
	Attempt string // a random id the page makes for each hand-over, so a repeat is recorded once
}

// ShareOffer is one message as Seyal offered it on the person's page: the item and its version, the
// message, who it was for, and a fingerprint (ShareDigest) of the ready words. The server signs it into
// the page and checks the signature when the record comes back (server/share.go), so a record is
// judged by what was offered at the time, not by the item as it is when the record arrives: a page
// left open, or a record that waited on an offline phone, still gets recorded once the item has moved
// on, and "edited" compares the words with the ones the person was really given.
type ShareOffer struct {
	Item    string `json:"i"`
	Share   string `json:"s"`
	To      string `json:"o"`
	Version int    `json:"v"`
	Digest  string `json:"d"`
}

// ShareDigest is the fingerprint of a share text (SHA-256, in hex): the page carries it signed, never
// anything secret. Line ends count as one kind ("\n"), as a browser's text box gives them back, so
// words the person didn't touch never read as changed.
func ShareDigest(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Offer is what the server signs into the page for one message the panel shows on item it.
func (m ShareText) Offer(it Item) ShareOffer {
	return ShareOffer{Item: it.ID, Share: m.ID, To: m.To, Version: it.Version, Digest: ShareDigest(m.Text)}
}

var attemptPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (r ShareRecord) check() error {
	switch {
	case r.ID == "":
		return &ShareInputError{"no message"}
	case !slices.Contains(shareRoutes, r.Route):
		return &ShareInputError{"unknown way of sharing"}
	case !attemptPattern.MatchString(r.Attempt):
		return &ShareInputError{"bad attempt id"}
	case !utf8.ValidString(r.Text) || strings.TrimSpace(r.Text) == "":
		return &ShareInputError{"no text"}
	case utf8.RuneCountInString(r.Text) > MaxShareText:
		return &ShareInputError{fmt.Sprintf("the text is over %d letters", MaxShareText)}
	}
	return nil
}

// Shares returns the messages the person may hand on from the item now: "Tell a colleague" always
// (anyone who can see the item may; it holds only what the item shows them), then the process's own
// messages offered in this step. Outside messages only for people who may change the item, and never
// on a closed one: speaking for the business to someone outside is a working act (D55).
// tx must come from db.WithTenant.
func Shares(ctx context.Context, tx pgx.Tx, it Item, by ledger.Actor, env ShareEnv) ([]ShareText, error) {
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return nil, err
	}
	return s.shares(it, by, env)
}

func (s session) shares(it Item, by ledger.Actor, env ShareEnv) ([]ShareText, error) {
	me, err := s.personName(actorPerson(by))
	if err != nil {
		return nil, err
	}
	outside := false
	if !engine.IsClosed(s.p, it.State) {
		a, err := s.actor(by)
		if err != nil {
			return nil, err
		}
		outside, _ = engine.Allowed(s.p, s.engineItem(it), nil, a)
	}
	out := []ShareText{{ID: playbook.BuiltinShare, Label: ColleagueLabel, To: playbook.ShareColleague,
		Text: colleagueText(it, env)}}
	for _, m := range s.p.Share {
		if !m.Offered(it.State) || (m.To == playbook.ShareOutside && !outside) {
			continue
		}
		text, empty, err := s.shareText(m.Text, it, env, me)
		if err != nil {
			return nil, err
		}
		phone, email, err := s.contact(m, it)
		if err != nil {
			return nil, err
		}
		out = append(out, ShareText{ID: m.ID, Label: strings.TrimSpace(m.Label), To: m.To, Text: text, Empty: empty,
			Phone: phone, Email: email})
	}
	return out, nil
}

// contact is the phone (as digits) and plain email of the list entry the message's contact: field
// points at on this item, read with the business's own list definition (its contact:). Each is "" when
// the message names no contact, the field is empty, the entry or its list has none, or the value can't
// be used safely in a link. Only an outside message has one (the process check refuses contact: on a
// colleague message; this is the second guard): a colleague message reaches everyone who can see the
// item and may carry its link.
func (s session) contact(m playbook.ShareMessage, it Item) (phone, email string, err error) {
	f := s.field(m.Contact)
	key, _ := it.Fields[m.Contact].(string)
	if m.To != playbook.ShareOutside || m.Contact == "" || f == nil || f.Type != "master" || key == "" {
		return "", "", nil
	}
	list, err := playbook.Kind(s.ctx, s.tx, f.Kind)
	if err != nil || list == nil || list.Contact == nil {
		return "", "", err
	}
	entry, err := master.Get(s.ctx, s.tx, f.Kind, key)
	if errors.Is(err, master.ErrNotFound) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	p, _ := entry.Fields[list.Contact.Phone].(string)
	e, _ := entry.Fields[list.Contact.Email].(string)
	return ChatDigits(p), PlainEmail(e), nil
}

var (
	phoneChars = regexp.MustCompile(`^\+[0-9\-(). ]*$`)
	nonDigits  = regexp.MustCompile(`[^0-9]`)
	// plainEmail is an address with nothing that could add to a mailto: link: no ?, &, %, comma, space,
	// line break or second @ (a value like "a@b.example?bcc=x@y.example" would add hidden receivers).
	plainEmail = regexp.MustCompile(`^[A-Za-z0-9._+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
)

// ChatDigits turns a phone number as people write it into the digits a chat link wants:
// "+91 00000 00000" → "910000000000". The number must start with + and its country code: a chat link
// reads the first digits as the country, so "00000 00000" written without it could open a stranger's
// chat in another country. It returns "" (the chat link then asks which contact) for a number without
// the +, with anything but digits, spaces, -, brackets and dots after it, or not 8 to 15 digits long.
func ChatDigits(phone string) string {
	if !phoneChars.MatchString(strings.TrimSpace(phone)) {
		return ""
	}
	digits := nonDigits.ReplaceAllString(phone, "")
	if len(digits) < 8 || len(digits) > 15 {
		return ""
	}
	return digits
}

// PlainEmail returns the address when it is a plain one (see plainEmail), else "": the Email button
// then opens with no address, never with one that adds receivers.
func PlainEmail(email string) string {
	if email = strings.TrimSpace(email); len(email) > 254 || !plainEmail.MatchString(email) {
		return ""
	}
	return email
}

// colleagueText is "Tell a colleague": the number, title, step and due date, and the item's link
// (which opens nothing without a login).
func colleagueText(it Item, env ShareEnv) string {
	parts := []string{it.Number}
	if title := strings.TrimSpace(it.Title); title != "" {
		parts = append(parts, title)
	}
	step := stepWords(it.State)
	if it.DueOn != "" {
		step += ", due " + shareDate(it.DueOn)
	}
	text := strings.Join(append(parts, step), " · ") + "."
	if env.Link != "" {
		text += "\nOpen in Seyal: " + env.Link
	}
	return text
}

// stepWords makes a step's key readable: "with_ops" → "with ops".
func stepWords(state string) string { return strings.ReplaceAll(state, "_", " ") }

// shareDate shows a date ("2026-11-12") as "12 Nov 2026": a message that leaves Seyal says the year.
func shareDate(date string) string {
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return date
	}
	return t.Format("2 Jan 2006")
}

// shareText fills a process's share text. An empty detail shows as "[Label: empty]", so the person
// sees it before handing the words on; the labels come back too, for the "1 detail is empty" hint.
func (s session) shareText(text string, it Item, env ShareEnv, me string) (string, []string, error) {
	builtin := map[string]string{"number": it.Number, "title": strings.TrimSpace(it.Title), "state": stepWords(it.State),
		"due": shareDate(it.DueOn), "link": env.Link, "me": me, "business": env.Business}
	if it.DueOn == "" {
		builtin["due"] = ""
	}
	var empty []string
	blank := func(label string) string {
		if !slices.Contains(empty, label) {
			empty = append(empty, label)
		}
		return "[" + label + ": empty]"
	}
	var firstErr error
	out := placeholder.ReplaceAllStringFunc(text, func(token string) string {
		key := token[1 : len(token)-1]
		if v, ok := builtin[key]; ok {
			if v == "" {
				return blank(capitalFirst(stepWords(key)))
			}
			return v
		}
		f := s.field(key)
		if f == nil {
			return token
		}
		v := it.Fields[key]
		if engine.IsEmpty(v) {
			return blank(cmp.Or(f.Label, capitalFirst(stepWords(key))))
		}
		shown, err := s.shareValue(*f, v, env.Currency)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return shown
	})
	return strings.TrimSpace(out), empty, firstErr
}

// shareValue shows a field's value in a share text: as display does, except money with the business's
// currency (Money) and dates with the month's name.
func (s session) shareValue(f playbook.Field, v any, currency string) (string, error) {
	switch x := v.(type) {
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			shown, err := s.shareValue(playbook.Field{Key: f.Key, Type: f.Of, Kind: f.Kind}, item, currency)
			if err != nil {
				return "", err
			}
			parts = append(parts, shown)
		}
		return strings.Join(parts, ", "), nil
	case float64:
		if f.Type == "money" {
			return Money(int64(math.Round(x)), currency), nil
		}
	case string:
		if f.Type == "date" {
			return shareDate(x), nil
		}
	}
	return s.display(f, v)
}

// capitalFirst gives a label a capital first letter: "due" → "Due".
func capitalFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// RecordShare records a hand-over the person's browser reported (item.shared). The server checks the
// same rules the panel used, not just stores what came. With offer (the message as the page was given
// it, its signature already checked by the caller), the rules are those of the moment Seyal offered
// the words: the record is kept even when the item has moved on since, and "edited" says whether the
// words differ from the ones offered. Without one (the server restarted since the page was drawn, so
// its signature can't be checked), the message must be one the person may hand on from this item now
// (Shares), and the record says only whether the words differ from Seyal's words for the item now
// ("differs"), not that the person changed them. It writes one event per attempt id and item, so a
// double tap or a script repeating it changes nothing (recorded is false then). The event carries the
// unchanged snapshot: the item's version, approvals, hand-over and step stay as they are.
// tx must come from db.WithTenant.
func RecordShare(ctx context.Context, tx pgx.Tx, itemID string, by ledger.Actor, rec ShareRecord, env ShareEnv,
	offer *ShareOffer) (recorded bool, err error) {
	if by.Kind != ledger.Human || by.PersonID == "" {
		return false, ErrShareRefused // only a person's own browser reports a hand-over
	}
	if err := rec.check(); err != nil {
		return false, err
	}
	it, err := getForUpdate(ctx, tx, itemID)
	if err != nil {
		return false, err
	}
	var done bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ledger
			WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
				AND kind = 'item.shared' AND body::jsonb #>> '{payload,change,attempt}' = $2)`,
		it.ID, rec.Attempt).Scan(&done); err != nil {
		return false, fmt.Errorf("workitem: share recorded: %w", err)
	}
	if done {
		return false, nil
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return false, err
	}
	change := map[string]any{"route": rec.Route, "text": rec.Text, "attempt": rec.Attempt}
	if offer != nil {
		if offer.Item != it.ID || offer.Share != rec.ID || offer.Version < 1 || offer.Version > it.Version ||
			s.shareTo(rec.ID) != offer.To {
			return false, ErrShareRefused
		}
		change["share"], change["to"], change["offered_version"] = offer.Share, offer.To, offer.Version
		change["edited"] = ShareDigest(rec.Text) != offer.Digest
		return true, s.record("item.shared", it, change, by)
	}
	list, err := s.shares(it, by, env)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(list, func(m ShareText) bool { return m.ID == rec.ID })
	if i < 0 {
		return false, ErrShareRefused
	}
	m := list[i]
	change["share"], change["to"], change["differs"] = m.ID, m.To, rec.Text != m.Text
	return true, s.record("item.shared", it, change, by)
}

// shareTo is who the message is for in the item's process version: "" when it has no such message.
func (s session) shareTo(id string) string {
	if id == playbook.BuiltinShare {
		return playbook.ShareColleague
	}
	for _, m := range s.p.Share {
		if m.ID == id {
			return m.To
		}
	}
	return ""
}
