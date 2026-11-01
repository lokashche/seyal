// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// field prepares one row of the side panel: the value people read, and the input to change it.
func (v *view) field(f playbook.Field, value any, open bool) (fieldView, error) {
	shown := v.show(f, value)
	fv := fieldView{Key: f.Key, Label: ask.Label(f), Value: shown,
		Editable: open && !f.Readonly, Input: "text"}
	if !fv.Editable {
		return fv, nil
	}
	switch f.Type {
	case "date":
		fv.Input, fv.Raw = "date", str(value)
	case "money":
		if x, ok := value.(float64); ok {
			fv.Raw = strconv.FormatFloat(x/100, 'f', -1, 64)
		}
	case "number":
		if x, ok := value.(float64); ok {
			fv.Raw = strconv.FormatFloat(x, 'f', -1, 64)
		}
	case "choice":
		fv.Input = "select"
		for _, o := range f.Options {
			fv.Options = append(fv.Options, option{Value: o, Label: o, Selected: o == value})
		}
	case "bool":
		fv.Input = "select"
		b, _ := value.(bool)
		fv.Options = []option{{"yes", "yes", value != nil && b}, {"no", "no", value != nil && !b}}
	case "person", "master":
		fv.Input = "select"
		opts, err := v.choices(f)
		if err != nil {
			return fv, err
		}
		for _, o := range opts {
			o.Selected = o.Value == value
			fv.Options = append(fv.Options, o)
		}
	case "list":
		if f.Of == "person" {
			fv.Input = "multi"
			opts, err := v.choices(playbook.Field{Type: "person"})
			if err != nil {
				return fv, err
			}
			chosen, _ := value.([]any)
			for _, o := range opts {
				o.Selected = slices.Contains(chosen, any(o.Value))
				fv.Options = append(fv.Options, o)
			}
		} else {
			fv.Input = "lines"
			items, _ := value.([]any)
			lines := make([]string, 0, len(items))
			for _, x := range items {
				lines = append(lines, str(x))
			}
			fv.Raw = strings.Join(lines, "\n")
		}
	default:
		fv.Raw = str(value)
	}
	return fv, nil
}

// choices lists the active people, or the active items of a master kind, sorted by name.
func (v *view) choices(f playbook.Field) ([]option, error) {
	sql := `SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant() AND status = 'active' ORDER BY display_name`
	args := []any{}
	if f.Type == "master" {
		sql = `SELECT key, name FROM master_items WHERE tenant_id = current_tenant() AND kind = $1 AND active ORDER BY name`
		args = append(args, f.Kind)
	}
	rows, _ := v.tx.Query(v.ctx, sql, args...)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ V, L string }])
	opts := make([]option, len(list))
	for i, o := range list {
		opts[i] = option{Value: o.V, Label: o.L}
	}
	return opts, err
}

// prefilled finds the fields whose value is still exactly what a master field pre-filled, so the
// side panel can mark them as suggestions ("from Nature of work").
func (v *view) prefilled(p *playbook.Playbook, it workitem.Item) (map[string]string, error) {
	from := map[string]string{}
	for _, f := range p.Fields {
		key, _ := it.Fields[f.Key].(string)
		if f.Type != "master" || len(f.Prefill) == 0 || key == "" {
			continue
		}
		m, err := master.Get(v.ctx, v.tx, f.Kind, key)
		if errors.Is(err, master.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for target, src := range f.Prefill {
			if val, ok := it.Fields[target]; ok && reflect.DeepEqual(val, m.Fields[src]) {
				from[target] = ask.Label(f)
			}
		}
	}
	return from, nil
}

// setField saves one field changed from the side panel. Values are understood the same way as
// chat answers ("12k", "15/11"), so the side panel and the chat always agree.
func (s *Server) setField(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.showScreen(w, r, c, id, http.StatusBadRequest, "That form couldn't be read.")
		return
	}
	key := r.PostForm.Get("key")
	var values []string
	for _, v := range r.PostForm["value"] {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		it, err := workitem.Get(r.Context(), tx, id)
		if err != nil {
			return err
		}
		p, err := workitem.PlaybookFor(r.Context(), tx, it)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(p.Fields, func(f playbook.Field) bool { return f.Key == key })
		if i < 0 {
			return errors.New("unknown field")
		}
		f := p.Fields[i]
		var value any // nil clears the field
		switch {
		case len(values) == 0:
		case f.Type == "list" && f.Of == "person":
			list := make([]any, len(values))
			for i, v := range values {
				list[i] = v
			}
			value = list
		case f.Type == "list":
			value, err = ask.Understand(f, strings.Join(values, "\n"), time.Now().In(c.tenant.Zone), ask.DBChoices(r.Context(), tx))
		default:
			value, err = ask.Understand(f, values[0], time.Now().In(c.tenant.Zone), ask.DBChoices(r.Context(), tx))
		}
		if err != nil {
			return errors.New(ask.Label(f) + ": " + err.Error())
		}
		if _, err = workitem.SetFields(r.Context(), tx, id, engine.Fields{key: value}, ledger.PersonActor(c.session.PersonID)); err != nil {
			return err
		}
		return ask.Nudge(r.Context(), tx, s.env(), id)
	})
	s.done(w, r, c, id, err)
}
