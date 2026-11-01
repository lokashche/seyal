// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seyal/internal/ask"
	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/people"
	"seyal/internal/playbook"
)

// Settings is the admin page for the things that change while the business runs: people and their
// roles, and the entries of each master-data kind. Its forms are built from the master-kind
// definitions, so a new kind gets its form without new code. Every change is a ledger event.

type settingsView struct {
	Tab       string
	Tabs      []tab
	People    *peopleView
	Roles     []roleRow
	Master    *masterView
	Processes *processList
	Process   *processView
	Lists     *listsView
	List      *listView
	AI        *aiView
}

type tab struct {
	Label, URL string
	Current    bool
}

type peopleView struct {
	Rows  []personRow
	Roles []option // all roles, for the "add a person" form
}

type personRow struct {
	ID, Name, Email string
	Active, Me      bool
	Roles           []option // all roles, Selected = held
	RoleNames       string
}

type roleRow struct{ Key, Name, Holders string }

type masterView struct {
	Kind, Label string
	New         []formField
	Items       []masterRow
}

type masterRow struct {
	Key, Name, Summary string
	Active             bool
	Form               []formField
}

type formField struct {
	Key, Label, Input, Raw string // Input: text, date, lines, select or multi
	Options                []option
}

type option struct {
	Value, Label string
	Selected     bool
}

// admin lets a request through only for people who hold the admin role.
func (s *Server) admin(h func(http.ResponseWriter, *http.Request, *call)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, c *call) {
		if !c.admin() {
			http.Error(w, "Only admins can open Settings.", http.StatusForbidden)
			return
		}
		h(w, r, c)
	})
}

func (s *Server) settingsHome(w http.ResponseWriter, r *http.Request, c *call) {
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/settings/people", http.StatusSeeOther)
}

func (s *Server) peoplePage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showSettings(w, r, c, "people", http.StatusOK, "", "")
}

func (s *Server) rolesPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showSettings(w, r, c, "roles", http.StatusOK, "", "")
}

func (s *Server) masterPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showSettings(w, r, c, r.PathValue("kind"), http.StatusOK, "", "")
}

var errNoTab = errors.New("no such settings page")

// showSettings renders one tab, with an optional notice (e.g. a new password) or problem.
func (s *Server) showSettings(w http.ResponseWriter, r *http.Request, c *call, tabKey string, status int, notice, problem string) {
	sv := settingsView{Tab: tabKey, Tabs: s.tabs(c, tabKey)}
	if tabKey != "people" && tabKey != "roles" {
		sv.Tabs = s.tabs(c, "lists") // a list's entries
	}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		switch tabKey {
		case "people":
			sv.People, err = v.people()
		case "roles":
			sv.Roles, err = v.roleRows()
		default:
			mk, err := playbook.Kind(r.Context(), tx, tabKey)
			if err != nil {
				return err
			}
			if mk == nil {
				return errNoTab
			}
			sv.Master, err = v.masterView(mk)
			return err
		}
		return err
	})
	if errors.Is(err, errNoTab) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := c.page(sv)
	p.Notice, p.Error = notice, problem
	s.render(w, r, status, "settings", p)
}

func (s *Server) tabs(c *call, current string) []tab {
	base := "/t/" + c.tenant.Slug + "/settings/"
	tabs := []tab{
		{Label: "People", URL: base + "people", Current: current == "people"},
		{Label: "Roles", URL: base + "roles", Current: current == "roles"},
		{Label: "Processes", URL: base + "processes", Current: current == "processes"},
		{Label: "Lists", URL: base + "lists", Current: current == "lists"},
		{Label: "AI usage", URL: base + "ai", Current: current == "ai"},
	}
	return tabs
}

// --- people and roles ---

func (v *view) people() (*peopleView, error) {
	roles, err := v.allRoles()
	if err != nil {
		return nil, err
	}
	rows, _ := v.tx.Query(v.ctx, `
		SELECT p.id::text, p.display_name, coalesce(p.login_email, ''), p.status = 'active',
			coalesce(array_agg(pr.role_key ORDER BY pr.role_key)
				FILTER (WHERE pr.role_key IS NOT NULL AND pr.revoked_at IS NULL), '{}')
		FROM persons p
		LEFT JOIN person_roles pr ON pr.tenant_id = p.tenant_id AND pr.person_id = p.id
		WHERE p.tenant_id = current_tenant()
		GROUP BY p.id ORDER BY p.status, p.display_name`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID, Name, Email string
		Active          bool
		Held            []string
	}])
	if err != nil {
		return nil, err
	}
	pv := &peopleView{Roles: roles}
	for _, p := range list {
		row := personRow{ID: p.ID, Name: p.Name, Email: p.Email, Active: p.Active, Me: p.ID == v.c.session.PersonID}
		var names []string
		for _, r := range roles {
			held := slices.Contains(p.Held, r.Value)
			row.Roles = append(row.Roles, option{Value: r.Value, Label: r.Label, Selected: held})
			if held {
				names = append(names, r.Label)
			}
		}
		row.RoleNames = strings.Join(names, ", ")
		pv.Rows = append(pv.Rows, row)
	}
	return pv, nil
}

func (v *view) allRoles() ([]option, error) {
	rows, _ := v.tx.Query(v.ctx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant() ORDER BY name`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ V, L string }])
	opts := make([]option, len(list))
	for i, o := range list {
		opts[i] = option{Value: o.V, Label: o.L}
	}
	return opts, err
}

func (v *view) roleRows() ([]roleRow, error) {
	rows, _ := v.tx.Query(v.ctx, `
		SELECT r.key, r.name, coalesce(string_agg(p.display_name, ', ' ORDER BY p.display_name), '')
		FROM roles r
		LEFT JOIN person_roles pr ON pr.tenant_id = r.tenant_id AND pr.role_key = r.key AND pr.revoked_at IS NULL
		LEFT JOIN persons p ON p.tenant_id = pr.tenant_id AND p.id = pr.person_id AND p.status = 'active'
		WHERE r.tenant_id = current_tenant()
		GROUP BY r.key, r.name ORDER BY r.name`)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[roleRow])
}

// addPerson adds someone, gives them the chosen roles and a first password, shown once.
func (s *Server) addPerson(w http.ResponseWriter, r *http.Request, c *call) {
	name, email := strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("email"))
	var password string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		by := ledger.PersonActor(c.session.PersonID)
		id, err := people.Add(r.Context(), tx, name, email, by)
		if err != nil {
			return err
		}
		for _, role := range r.PostForm["role"] {
			if _, err := people.Grant(r.Context(), tx, id, role, by); err != nil {
				return err
			}
		}
		password, err = auth.NewPassword(r.Context(), tx, id, by)
		return err
	})
	if err != nil {
		s.settingsProblem(w, r, c, "people", err)
		return
	}
	s.showSettings(w, r, c, "people", http.StatusOK,
		fmt.Sprintf("Added %s. Their password (shown only now): %s", name, password), "")
}

// updatePerson saves name, email, roles and active/disabled. Admins can't lock themselves out, and
// at least one active admin always remains.
func (s *Server) updatePerson(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.showSettings(w, r, c, "people", http.StatusBadRequest, "", "That form couldn't be read.")
		return
	}
	active := r.PostForm.Get("status") == "active"
	wanted := r.PostForm["role"]
	if id == c.session.PersonID && (!active || !slices.Contains(wanted, adminRole)) {
		s.showSettings(w, r, c, "people", http.StatusUnprocessableEntity, "",
			"You can't disable yourself or remove your own admin role. Ask another admin.")
		return
	}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		by := ledger.PersonActor(c.session.PersonID)
		if _, err := people.Update(r.Context(), tx, id, r.PostForm.Get("name"), r.PostForm.Get("email"), by); err != nil {
			return err
		}
		all, err := (&view{ctx: r.Context(), tx: tx}).allRoles()
		if err != nil {
			return err
		}
		for _, role := range all {
			if slices.Contains(wanted, role.Value) {
				_, err = people.Grant(r.Context(), tx, id, role.Value, by)
			} else {
				_, err = people.Revoke(r.Context(), tx, id, role.Value, by)
			}
			if err != nil {
				return err
			}
		}
		if _, err := people.SetActive(r.Context(), tx, id, active, by); err != nil {
			return err
		}
		var admins int
		if err := tx.QueryRow(r.Context(), `
			SELECT count(*) FROM person_roles pr JOIN persons p ON p.tenant_id = pr.tenant_id AND p.id = pr.person_id
			WHERE pr.tenant_id = current_tenant() AND pr.role_key = $1 AND pr.revoked_at IS NULL AND p.status = 'active'`,
			adminRole).Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return errors.New("at least one active admin must remain")
		}
		return nil
	})
	if err != nil {
		s.settingsProblem(w, r, c, "people", err)
		return
	}
	s.showSettings(w, r, c, "people", http.StatusOK, "Saved.", "")
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request, c *call) {
	var name, password string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT display_name FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
			r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		var err error
		password, err = auth.NewPassword(r.Context(), tx, r.PathValue("id"), ledger.PersonActor(c.session.PersonID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.settingsProblem(w, r, c, "people", err)
		return
	}
	s.showSettings(w, r, c, "people", http.StatusOK,
		fmt.Sprintf("New password for %s (shown only now): %s", name, password), "")
}

var (
	roleKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	nonKey  = regexp.MustCompile(`[^a-z0-9]+`)
)

func (s *Server) addRole(w http.ResponseWriter, r *http.Request, c *call) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	key := strings.Trim(nonKey.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if name == "" || !roleKey.MatchString(key) {
		s.showSettings(w, r, c, "roles", http.StatusUnprocessableEntity, "", "Please give the role a name that starts with a letter.")
		return
	}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		_, err := people.DefineRole(r.Context(), tx, key, name, ledger.PersonActor(c.session.PersonID))
		return err
	})
	if err != nil {
		s.settingsProblem(w, r, c, "roles", err)
		return
	}
	s.showSettings(w, r, c, "roles", http.StatusOK, "Role "+name+" saved.", "")
}

// --- master data ---

func (v *view) masterView(mk *playbook.MasterKind) (*masterView, error) {
	mv := &masterView{Kind: mk.Kind, Label: mk.Label}
	var err error
	if mv.New, err = v.form(mk, nil); err != nil {
		return nil, err
	}
	rows, _ := v.tx.Query(v.ctx, `
		SELECT key, name, fields, active FROM master_items
		WHERE tenant_id = current_tenant() AND kind = $1 ORDER BY active DESC, name`, mk.Kind)
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Key, Name string
		Fields    engine.Fields
		Active    bool
	}])
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		row := masterRow{Key: it.Key, Name: it.Name, Active: it.Active}
		var parts []string
		for _, f := range mk.Fields {
			if shown := v.show(f, it.Fields[f.Key]); shown != "" && len(parts) < 3 {
				parts = append(parts, f.Label+": "+shown)
			}
		}
		row.Summary = strings.Join(parts, " · ")
		if row.Form, err = v.form(mk, it.Fields); err != nil {
			return nil, err
		}
		mv.Items = append(mv.Items, row)
	}
	return mv, nil
}

// form builds the inputs for a master kind's fields, filled with values (nil for a new item).
func (v *view) form(mk *playbook.MasterKind, values engine.Fields) ([]formField, error) {
	var out []formField
	for _, f := range mk.Fields {
		ff, err := v.input(f, values[f.Key])
		if err != nil {
			return nil, err
		}
		out = append(out, ff)
	}
	return out, nil
}

// input prepares one form input for a field, showing its current value.
func (v *view) input(f playbook.Field, value any) (formField, error) {
	ff := formField{Key: f.Key, Label: ask.Label(f), Input: "text"}
	switch f.Type {
	case "date":
		ff.Input, ff.Raw = "date", str(value)
	case "money":
		if x, ok := value.(float64); ok {
			ff.Raw = strconv.FormatFloat(x/100, 'f', -1, 64)
		}
	case "number":
		if x, ok := value.(float64); ok {
			ff.Raw = strconv.FormatFloat(x, 'f', -1, 64)
		}
	case "choice":
		ff.Input = "select"
		for _, o := range f.Options {
			ff.Options = append(ff.Options, option{Value: o, Label: o, Selected: o == value})
		}
	case "bool":
		ff.Input = "select"
		b, set := value.(bool)
		ff.Options = []option{{"yes", "yes", set && b}, {"no", "no", set && !b}}
	case "person":
		ff.Input = "select"
		people, err := v.activePeople()
		if err != nil {
			return ff, err
		}
		for _, o := range people {
			o.Selected = o.Value == value
			ff.Options = append(ff.Options, o)
		}
	case "list":
		items, _ := value.([]any)
		if f.Of == "person" {
			ff.Input = "multi"
			people, err := v.activePeople()
			if err != nil {
				return ff, err
			}
			for _, o := range people {
				o.Selected = slices.Contains(items, any(o.Value))
				ff.Options = append(ff.Options, o)
			}
		} else {
			ff.Input = "lines"
			lines := make([]string, 0, len(items))
			for _, x := range items {
				lines = append(lines, fmt.Sprint(x))
			}
			ff.Raw = strings.Join(lines, "\n")
		}
	default:
		ff.Raw = str(value)
	}
	return ff, nil
}

func (v *view) activePeople() ([]option, error) {
	rows, _ := v.tx.Query(v.ctx, `
		SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant() AND status = 'active'
		ORDER BY display_name`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ V, L string }])
	opts := make([]option, len(list))
	for i, o := range list {
		opts[i] = option{Value: o.V, Label: o.L}
	}
	return opts, err
}

// saveMaster adds, edits, retires or brings back a master item. A new item's key is made from its
// name ("Rasi Textiles" → "rasi-textiles"), with a number added if that key is taken.
func (s *Server) saveMaster(w http.ResponseWriter, r *http.Request, c *call) {
	kind := r.PathValue("kind")
	if err := r.ParseForm(); err != nil {
		s.showSettings(w, r, c, kind, http.StatusBadRequest, "", "That form couldn't be read.")
		return
	}
	key, name, action := r.PostForm.Get("key"), strings.TrimSpace(r.PostForm.Get("name")), r.PostForm.Get("action")
	notice := ""
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		by := ledger.PersonActor(c.session.PersonID)
		mk, err := playbook.Kind(r.Context(), tx, kind)
		if err != nil {
			return err
		}
		if mk == nil {
			return errNoTab
		}
		switch action {
		case "retire":
			notice = name + " retired: it can't be chosen for new work any more. Existing work keeps it."
			return master.Deactivate(r.Context(), tx, mk.Kind, key, by)
		case "restore":
			it, err := master.Get(r.Context(), tx, mk.Kind, key)
			if err != nil {
				return err
			}
			notice = it.Name + " is back in use."
			fields := engine.Fields{} // only the details the list still records
			for _, f := range mk.Fields {
				if v, ok := it.Fields[f.Key]; ok {
					fields[f.Key] = v
				}
			}
			_, _, err = master.Save(r.Context(), tx, mk, key, it.Name, fields, by)
			return err
		}
		if key == "" {
			var err error
			if key, err = master.FreeKey(r.Context(), tx, mk.Kind, name); err != nil {
				return err
			}
		}
		fields := engine.Fields{}
		for _, f := range mk.Fields {
			v, err := parseValue(f, r.PostForm["f."+f.Key], time.Now().In(c.tenant.Zone), ask.DBChoices(r.Context(), tx))
			if err != nil {
				return fmt.Errorf("%s: %w", ask.Label(f), err)
			}
			if v != nil {
				fields[f.Key] = v
			}
		}
		notice = name + " saved."
		_, _, err = master.Save(r.Context(), tx, mk, key, name, fields, by)
		return err
	})
	if errors.Is(err, errNoTab) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.settingsProblem(w, r, c, kind, err)
		return
	}
	s.showSettings(w, r, c, kind, http.StatusOK, notice, "")
}

// parseValue turns a form input into a stored value, understood like a chat answer ("12k",
// "15/11"). Empty means "no value".
func parseValue(f playbook.Field, raw []string, today time.Time, choices ask.Choices) (any, error) {
	var vals []string
	for _, v := range raw {
		if v = strings.TrimSpace(v); v != "" {
			vals = append(vals, v)
		}
	}
	switch {
	case len(vals) == 0:
		return nil, nil
	case f.Type == "list" && f.Of == "person":
		list := make([]any, len(vals))
		for i, v := range vals {
			list[i] = v
		}
		return list, nil
	case f.Type == "list":
		return ask.Understand(f, strings.Join(vals, "\n"), today, choices)
	}
	return ask.Understand(f, vals[0], today, choices)
}

// settingsProblem shows a refusal in plain words; database errors are logged and shown generally.
func (s *Server) settingsProblem(w http.ResponseWriter, r *http.Request, c *call, tabKey string, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" { // unique violation
			s.showSettings(w, r, c, tabKey, http.StatusUnprocessableEntity, "", "That is already in use.")
			return
		}
		s.fail(w, r, err)
		return
	}
	s.showSettings(w, r, c, tabKey, http.StatusUnprocessableEntity, "", capital(err.Error())+".")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
