// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/workitem"
)

// Moving someone's open work on Tell Seyal (D75, M3). movewords.go reads the words; this file finds the
// two people and lists the work. The list is an offer: it is shown once and writes nothing (like an
// answer to a question, D67). Nothing moves until an admin presses its button, typed or spoken, and
// then workitem.Reassign checks everything again.

// MoveShown is how many items the list shows before "and 20 more" (Pick shows them all).
const MoveShown = 20

// MoveOffer is Seyal's answer to "move Ben's work to Dev".
type MoveOffer struct {
	Said     string       // what the person wrote; "" when the list was opened from a button
	Line     string       // Seyal's words: "Ben has 3 open items:", or why nothing can move
	From, To Choice       // the two people, once Seyal knows them
	Items    []MoveItem   // From's open work, by number
	Also     string       // "Ben is also Maker on 1 of these; change that in the side panel if needed."
	Choose   []MoveChoice // "Which one do you mean?": one button per pair of people
}

// MoveItem is one item in the list.
type MoveItem struct {
	ID, Number, Title, Step string
	Ticked                  bool   // in "Move all"; false when the receiver could only pass it on
	Why                     string // why it starts unticked
}

// MoveChoice is a button for "which one do you mean?": it opens the list for these two people.
type MoveChoice struct{ Label, From, To string }

// ReadMove reads a message on Tell Seyal as "move Ben's work to Dev". It returns nil when the message
// isn't one (the usual routing then reads it): not one of the shapes, the person whose work it is is
// nobody here (often a name from the business's lists), or the receiver is nobody here (an admin
// is told "I don't know Deb here" instead, when it could be a name). Otherwise
// it returns Seyal's answer: the list, a "which one?", or why it can't. tx must come from db.WithTenant.
func ReadMove(ctx context.Context, tx pgx.Tx, personID, text string) (*MoveOffer, error) {
	fromWords, toWords, ok := readMoveWords(text)
	if !ok {
		return nil, nil
	}
	rows, _ := tx.Query(ctx, `SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant() ORDER BY display_name, id`)
	people, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Choice])
	if err != nil {
		return nil, fmt.Errorf("ask: people: %w", err)
	}
	admin, err := isAdmin(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	from, to := whoIsNamed(fromWords, people), whoIsNamed(toWords, people)
	said := strings.TrimSpace(text)
	if len(from) == 0 {
		// Not someone here: often a name on a list, as in "Pass Sundaram's papers to Dev", which is
		// new work. The usual routing reads it, for everyone.
		return nil, nil
	}
	if len(to) == 0 {
		if admin && mayBeName(toWords) {
			return &MoveOffer{Said: said, Line: fmt.Sprintf("I don't know %s here.", toWords)}, nil
		}
		return nil, nil // an ordinary sentence after all
	}
	if !admin {
		return &MoveOffer{Said: said, Line: sentenceOf(workitem.ErrNotAdmin.Error())}, nil
	}
	if len(from) > 1 || len(to) > 1 {
		o := &MoveOffer{Said: said, Line: "Which one do you mean?"}
		for _, f := range from {
			for _, t := range to {
				label := f.Name + " → " + t.Name
				switch {
				case len(to) == 1:
					label = f.Name
				case len(from) == 1:
					label = t.Name
				}
				o.Choose = append(o.Choose, MoveChoice{Label: label, From: f.ID, To: t.ID})
			}
		}
		return o, nil
	}
	o, err := OfferMove(ctx, tx, personID, from[0].ID, to[0].ID)
	if o != nil {
		o.Said = said
	}
	return o, err
}

// OfferMove lists the open work of person from, to move to person to, as Seyal shows it to personID:
// only an admin gets the list. An item to couldn't work (a step of a role they don't have) starts
// unticked, so "Move all" never leaves work stuck. tx must come from db.WithTenant.
func OfferMove(ctx context.Context, tx pgx.Tx, personID, from, to string) (*MoveOffer, error) {
	admin, err := isAdmin(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	if !admin {
		return &MoveOffer{Line: sentenceOf(workitem.ErrNotAdmin.Error())}, nil
	}
	names := map[string]string{}
	rows, _ := tx.Query(ctx, `SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant() AND id::text = ANY($1)`,
		[]string{from, to})
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Choice])
	if err != nil {
		return nil, fmt.Errorf("ask: people: %w", err)
	}
	for _, p := range found {
		names[p.ID] = p.Name
	}
	if names[from] == "" || names[to] == "" {
		return &MoveOffer{Line: "I don't know that person here."}, nil
	}
	o := &MoveOffer{From: Choice{ID: from, Name: names[from]}, To: Choice{ID: to, Name: names[to]}}
	if err := workitem.MayMove(ctx, tx, from, to); err != nil {
		o.Line = sentenceOf(err.Error()) // "Dev is no longer active, so they can't receive work."
		return o, nil
	}
	items, err := workitem.OpenWorkOf(ctx, tx, from)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		o.Line = names[from] + " has no open work."
		return o, nil
	}
	members, err := workitem.Members(ctx, tx)
	if err != nil {
		return nil, err
	}
	var toRoles []string
	if i := slices.IndexFunc(members, func(m workitem.Member) bool { return m.ID == to }); i >= 0 {
		toRoles = members[i].Roles
	}
	roleNames, err := roleNamesOf(ctx, tx)
	if err != nil {
		return nil, err
	}
	also := map[string]int{} // a person field's label → on how many of the items it names from
	for _, it := range items {
		p, err := workitem.PlaybookFor(ctx, tx, it)
		if err != nil {
			return nil, err
		}
		mi := MoveItem{ID: it.ID, Number: it.Number, Title: it.Title, Step: capitalFirst(human(it.State)), Ticked: true}
		kind, owner := engine.Owner(p, it.State)
		if kind == "role" && !slices.Contains(toRoles, owner) {
			mi.Ticked = false
			mi.Why = fmt.Sprintf("%s isn't in %s, so they could only pass this on.", names[to], cmp.Or(roleNames[owner], owner))
		}
		o.Items = append(o.Items, mi)
		for _, f := range p.Fields {
			if id, _ := it.Fields[f.Key].(string); f.Type == "person" && id == from && !(kind == "field" && f.Key == owner) {
				also[Label(f)]++
			}
		}
	}
	o.Line = fmt.Sprintf("%s has %d open items:", names[from], len(items))
	if len(items) == 1 {
		o.Line = names[from] + " has 1 open item:"
	}
	if len(also) > 0 {
		var parts []string
		for _, label := range slices.Sorted(maps.Keys(also)) {
			parts = append(parts, fmt.Sprintf("%s on %d", label, also[label]))
		}
		o.Also = fmt.Sprintf("%s is also %s of these; change it in the side panel if needed.",
			names[from], strings.Join(parts, " and "))
	}
	return o, nil
}

// isAdmin reports whether the person is an admin of this business now.
func isAdmin(ctx context.Context, tx pgx.Tx, personID string) (bool, error) {
	var admin bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM person_roles r JOIN persons p ON p.tenant_id = r.tenant_id AND p.id = r.person_id
			WHERE r.tenant_id = current_tenant() AND r.person_id::text = $1 AND r.role_key = $2
				AND r.revoked_at IS NULL AND p.status = 'active')`, personID, workitem.AdminRole).Scan(&admin)
	if err != nil {
		return false, fmt.Errorf("ask: admin: %w", err)
	}
	return admin, nil
}

// roleNamesOf maps the business's role keys to their names.
func roleNamesOf(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, _ := tx.Query(ctx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant()`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Key, Name string }])
	if err != nil {
		return nil, fmt.Errorf("ask: roles: %w", err)
	}
	out := make(map[string]string, len(list))
	for _, r := range list {
		out[r.Key] = r.Name
	}
	return out, nil
}

// capitalFirst starts words with a capital letter: "with ops" → "With ops".
func capitalFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
