// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/people"
	"seyal/internal/playbook"
)

// Settings → import from a spreadsheet, and download as one. A spreadsheet comes as CSV (Excel and
// Google Sheets both save it), read with the standard library. The first row names the columns: Name
// and the details of the list (by label or key, in any order and case). An import is shown first, row
// by row (new, changed, unchanged, problems); nothing is saved until the person confirms, and then
// all rows are saved together or none. Each row is saved exactly like a form in Settings, so every
// change is checked and goes into the ledger. Rows are matched to existing entries by name (people by
// email). An empty cell keeps what is stored; an import never retires, disables or takes roles away.

const (
	maxSheetBytes = 1 << 20
	maxSheetRows  = 2000
)

// sheet is a spreadsheet's header row and data rows; line is the row number people see in the file.
type sheet struct {
	head []string
	rows []sheetRow
}

type sheetRow struct {
	line  int
	cells []string
}

// cell returns column i of the row, trimmed, or "" if the row is shorter. The ' that a download puts
// before a cell starting like a formula is removed.
func (r sheetRow) cell(i int) string {
	if i < 0 || i >= len(r.cells) {
		return ""
	}
	c := strings.TrimSpace(r.cells[i])
	if len(c) > 1 && c[0] == '\'' && strings.ContainsRune(formulaStart, rune(c[1])) {
		c = c[1:]
	}
	return c
}

// readSheet reads CSV text. It drops the byte-order mark Excel adds, accepts ";" between columns
// (what Excel writes in many countries), and skips empty rows.
func readSheet(text string) (*sheet, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	first, _, _ := strings.Cut(text, "\n")
	cr := csv.NewReader(strings.NewReader(text))
	if strings.Count(first, ";") > strings.Count(first, ",") {
		cr.Comma = ';'
	}
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	sh := &sheet{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the file couldn't be read as CSV (%v); in Excel or Google Sheets, save or download it as CSV", err)
		}
		line, _ := cr.FieldPos(0)
		if !slices.ContainsFunc(rec, func(c string) bool { return strings.TrimSpace(c) != "" }) {
			continue
		}
		if sh.head == nil {
			sh.head = rec
			continue
		}
		if len(sh.rows) == maxSheetRows {
			return nil, fmt.Errorf("the file has more than %d rows; please import it in parts", maxSheetRows)
		}
		sh.rows = append(sh.rows, sheetRow{line: line, cells: rec})
	}
	if sh.head == nil {
		return nil, errors.New("the file is empty")
	}
	return sh, nil
}

var notWord = regexp.MustCompile(`[^\pL\pN]+`)

// heading makes a column heading comparable: "Est. hours (₹)", "est_hours" and "EST HOURS" all
// become "est hours" (a currency sign isn't a letter, so it goes too).
func heading(s string) string {
	return strings.TrimSpace(notWord.ReplaceAllString(strings.ToLower(s), " "))
}

// column finds the first column whose heading is one of names, or -1.
func (sh *sheet) column(names ...string) int {
	for i, h := range sh.head {
		if slices.Contains(names, heading(h)) {
			return i
		}
	}
	return -1
}

// importView is the preview of an import, or what it did.
type importView struct {
	Title, Back, Action string
	CSV                 string   // the file's text, sent again when the person confirms
	Columns, Ignored    []string // headings used, and headings that match nothing
	Rows                []importRow
	New, Changed, Same  int
	Problems            int
}

type importRow struct {
	Line                 int
	Name, Status, Detail string
	Problem              bool
}

// ToSave is how many rows confirming would save.
func (iv *importView) ToSave() int { return iv.New + iv.Changed }

// Pill is the status colour of a row.
func (r importRow) Pill() string {
	switch r.Status {
	case "new":
		return "p-ok"
	case "changed":
		return "p-wait"
	case "unchanged":
		return "p-closed"
	}
	return "p-late"
}

// sortRows puts problems first, then new, changed and unchanged rows, each in file order.
func (iv *importView) sortRows() {
	order := map[string]int{"problem": 0, "new": 1, "changed": 2, "unchanged": 3}
	slices.SortStableFunc(iv.Rows, func(a, b importRow) int { return order[a.Status] - order[b.Status] })
}

func (iv *importView) add(line int, name, status, detail string) {
	iv.Rows = append(iv.Rows, importRow{Line: line, Name: name, Status: status, Detail: detail, Problem: status == "problem"})
	switch status {
	case "new":
		iv.New++
	case "changed":
		iv.Changed++
	case "unchanged":
		iv.Same++
	default:
		iv.Problems++
	}
}

// sheetText returns the uploaded file (field "sheet"), or the text sent back with a confirmation
// (field "csv").
func sheetText(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSheetBytes+64<<10)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(maxSheetBytes); err != nil {
			return "", errors.New("the file is too big (1 MB at most) or couldn't be read")
		}
		f, _, err := r.FormFile("sheet")
		if err != nil {
			return "", errors.New("please choose a CSV file")
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxSheetBytes+1))
		if err != nil || len(b) > maxSheetBytes {
			return "", errors.New("the file is too big (1 MB at most) or couldn't be read")
		}
		return string(b), nil
	}
	if err := r.ParseForm(); err != nil {
		return "", errors.New("that form couldn't be read")
	}
	return r.PostForm.Get("csv"), nil
}

func (s *Server) showImport(w http.ResponseWriter, r *http.Request, c *call, tabKey string, iv *importView) {
	iv.sortRows()
	sv := settingsView{Tab: tabKey, Tabs: s.tabs(c, tabKey), Import: iv}
	s.render(w, r, http.StatusOK, "settings", c.page(sv))
}

// --- list entries ---

// plannedEntry is one row of a list import: what will be saved, if there are no problems.
type plannedEntry struct {
	key, name string // key is "" for a new entry
	fields    engine.Fields
}

func (v *view) planEntries(mk *playbook.MasterKind, sh *sheet, iv *importView) ([]plannedEntry, error) {
	nameCol := sh.column("name", heading(listLabel(mk)), heading(listLabel(mk))+" name")
	if nameCol < 0 {
		return nil, errors.New(`the first row needs a "Name" column`)
	}
	cols := map[int]playbook.Field{}
	for i, h := range sh.head {
		if i == nameCol {
			iv.Columns = append(iv.Columns, "Name")
			continue
		}
		at := slices.IndexFunc(mk.Fields, func(f playbook.Field) bool {
			return heading(h) == heading(f.Key) || heading(h) == heading(ask.Label(f))
		})
		switch {
		case at >= 0:
			cols[i] = mk.Fields[at]
			iv.Columns = append(iv.Columns, ask.Label(mk.Fields[at]))
		case strings.TrimSpace(h) != "":
			iv.Ignored = append(iv.Ignored, h)
		}
	}

	rows, _ := v.tx.Query(v.ctx, `
		SELECT key, name, fields, active FROM master_items WHERE tenant_id = current_tenant() AND kind = $1`, mk.Kind)
	stored, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Key, Name string
		Fields    engine.Fields
		Active    bool
	}])
	if err != nil {
		return nil, err
	}
	byName := map[string]int{}
	for i, it := range stored {
		byName[strings.ToLower(it.Name)] = i
	}

	today, choices := time.Now().In(v.c.tenant.Zone), ask.DBChoices(v.ctx, v.tx)
	seen := map[string]int{}
	var plan []plannedEntry
	for _, row := range sh.rows {
		name := strings.Join(strings.Fields(row.cell(nameCol)), " ")
		if name == "" {
			iv.add(row.line, "", "problem", "This row has no name.")
			continue
		}
		if first, dup := seen[strings.ToLower(name)]; dup {
			iv.add(row.line, name, "problem", fmt.Sprintf("Also on row %d; each entry once, please.", first))
			continue
		}
		seen[strings.ToLower(name)] = row.line

		pe := plannedEntry{name: name, fields: engine.Fields{}}
		i, exists := byName[strings.ToLower(name)]
		if exists {
			pe.key = stored[i].Key
			for _, f := range mk.Fields { // only details the list still records
				if x, ok := stored[i].Fields[f.Key]; ok {
					pe.fields[f.Key] = x
				}
			}
		}
		var problems, changed []string
		for col, f := range cols {
			raw := row.cell(col)
			if raw == "" {
				continue
			}
			x, err := ask.Understand(f, raw, today, choices)
			if err == nil {
				err = engine.CheckValue(f, x)
			}
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s", ask.Label(f), err))
				continue
			}
			pe.fields[f.Key] = x
		}
		if len(problems) > 0 {
			slices.Sort(problems)
			iv.add(row.line, name, "problem", capital(strings.Join(problems, "; "))+".")
			continue
		}
		switch {
		case !exists:
			iv.add(row.line, name, "new", "")
		default:
			old := stored[i]
			for _, f := range mk.Fields {
				if !sameValue(old.Fields[f.Key], pe.fields[f.Key]) {
					changed = append(changed, ask.Label(f))
				}
			}
			if old.Name != name {
				changed = append([]string{"name"}, changed...)
			}
			detail := strings.Join(changed, ", ")
			if !old.Active {
				detail = strings.TrimPrefix(detail+", brought back into use", ", ")
			}
			if detail == "" {
				iv.add(row.line, name, "unchanged", "")
				continue
			}
			iv.add(row.line, name, "changed", capital(detail)+".")
		}
		plan = append(plan, pe)
	}
	return plan, nil
}

// sameValue reports whether two stored values are the same.
func sameValue(a, b any) bool {
	ha, errA := engine.HashFields(engine.Fields{"v": a})
	hb, errB := engine.HashFields(engine.Fields{"v": b})
	return errA == nil && errB == nil && ha == hb
}

func (s *Server) importEntries(w http.ResponseWriter, r *http.Request, c *call) {
	kind := r.PathValue("kind")
	text, err := sheetText(w, r)
	if err != nil {
		s.settingsProblem(w, r, c, kind, err)
		return
	}
	apply := r.FormValue("action") == "apply"
	var iv *importView
	err = db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		mk, err := playbook.Kind(r.Context(), tx, kind)
		if err != nil {
			return err
		}
		if mk == nil {
			return errNoTab
		}
		sh, err := readSheet(text)
		if err != nil {
			return err
		}
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		base := "/t/" + c.tenant.Slug + "/settings/master/" + kind
		iv = &importView{Title: "Import " + listLabel(mk) + "s", Back: base, Action: base + "/import", CSV: text}
		plan, err := v.planEntries(mk, sh, iv)
		if err != nil || !apply || iv.Problems > 0 {
			return err
		}
		by := ledger.PersonActor(c.session.PersonID)
		for _, pe := range plan {
			if pe.key == "" {
				if pe.key, err = master.FreeKey(r.Context(), tx, kind, pe.name); err != nil {
					return err
				}
			}
			if _, _, err := master.Save(r.Context(), tx, mk, pe.key, pe.name, pe.fields, by); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoTab):
		http.NotFound(w, r)
	case err != nil:
		s.settingsProblem(w, r, c, kind, err)
	case apply && iv.Problems == 0:
		s.showSettings(w, r, c, kind, http.StatusOK, imported(iv), "")
	default:
		s.showImport(w, r, c, "lists", iv)
	}
}

func imported(iv *importView) string {
	return fmt.Sprintf("Imported: %d new, %d changed, %d unchanged.", iv.New, iv.Changed, iv.Same)
}

// exportEntries downloads a list's entries in use as CSV, in the shape an import reads, so a list
// can be edited in a spreadsheet and brought back.
func (s *Server) exportEntries(w http.ResponseWriter, r *http.Request, c *call) {
	kind := r.PathValue("kind")
	var out bytes.Buffer
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		mk, err := playbook.Kind(r.Context(), tx, kind)
		if err != nil {
			return err
		}
		if mk == nil {
			return errNoTab
		}
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		rows, _ := tx.Query(r.Context(), `
			SELECT name, fields FROM master_items WHERE tenant_id = current_tenant() AND kind = $1 AND active ORDER BY name`, kind)
		items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			Name   string
			Fields engine.Fields
		}])
		if err != nil {
			return err
		}
		table := [][]string{{"Name"}}
		for _, f := range mk.Fields {
			table[0] = append(table[0], ask.Label(f))
		}
		for _, it := range items {
			line := []string{it.Name}
			for _, f := range mk.Fields {
				line = append(line, v.cell(f, it.Fields[f.Key]))
			}
			table = append(table, line)
		}
		return writeCSV(&out, table)
	})
	if errors.Is(err, errNoTab) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sendCSV(w, kind, out.Bytes())
}

// cell writes a stored value the way an import reads it back: amounts as plain numbers, dates as
// 2026-11-15, people and entries by name, list items separated by "; ".
func (v *view) cell(f playbook.Field, value any) string {
	switch x := value.(type) {
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			if s := v.cell(playbook.Field{Key: f.Key, Type: f.Of, Kind: f.Kind}, item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	case float64:
		if f.Type == "money" {
			x /= 100
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		if f.Type == "date" {
			return x
		}
	}
	return v.show(f, value)
}

// formulaStart are the first characters that make a spreadsheet treat a cell as a formula.
const formulaStart = "=+-@\t\r"

func writeCSV(out *bytes.Buffer, table [][]string) error {
	out.WriteString("\ufeff") // so Excel reads names in any script correctly
	// A name like "=HYPERLINK(\u2026)" typed into a list must stay text when the file is opened in a
	// spreadsheet, not run as a formula: a leading ' makes it text, and an import removes it again.
	for _, line := range table {
		for i, c := range line {
			if c != "" && strings.ContainsRune(formulaStart, rune(c[0])) {
				line[i] = "'" + c
			}
		}
	}
	cw := csv.NewWriter(out)
	cw.UseCRLF = true
	if err := cw.WriteAll(table); err != nil {
		return err
	}
	return cw.Error()
}

func sendCSV(w http.ResponseWriter, name string, body []byte) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`, name, time.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// --- people ---

// plannedPerson is one row of a people import.
type plannedPerson struct {
	id, name, email string // id is "" for someone new
	roles           []string
}

var roleSeparator = regexp.MustCompile(`\s*[,;\n]\s*`)

func (v *view) planPeople(sh *sheet, iv *importView) ([]plannedPerson, error) {
	nameCol, emailCol := sh.column("name"), sh.column("email", "login email", "e mail")
	roleCol := sh.column("roles", "role")
	if nameCol < 0 || emailCol < 0 {
		return nil, errors.New(`the first row needs "Name" and "Email" columns (and "Roles" if you like)`)
	}
	for i, h := range sh.head {
		switch i {
		case nameCol:
			iv.Columns = append(iv.Columns, "Name")
		case emailCol:
			iv.Columns = append(iv.Columns, "Email")
		case roleCol:
			iv.Columns = append(iv.Columns, "Roles")
		default:
			if strings.TrimSpace(h) != "" {
				iv.Ignored = append(iv.Ignored, h)
			}
		}
	}
	roles, err := v.allRoles()
	if err != nil {
		return nil, err
	}
	rows, _ := v.tx.Query(v.ctx, `
		SELECT p.id::text, p.display_name, p.login_email, p.status = 'active',
			coalesce(array_agg(pr.role_key) FILTER (WHERE pr.role_key IS NOT NULL AND pr.revoked_at IS NULL), '{}')
		FROM persons p
		LEFT JOIN person_roles pr ON pr.tenant_id = p.tenant_id AND pr.person_id = p.id
		WHERE p.tenant_id = current_tenant() AND p.login_email IS NOT NULL
		GROUP BY p.id`)
	stored, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID, Name, Email string
		Active          bool
		Held            []string
	}])
	if err != nil {
		return nil, err
	}

	seen := map[string]int{}
	var plan []plannedPerson
	for _, row := range sh.rows {
		name := strings.Join(strings.Fields(row.cell(nameCol)), " ")
		email := strings.ToLower(row.cell(emailCol))
		label := cmp.Or(name, email)
		var problems []string
		if name == "" {
			problems = append(problems, "no name")
		}
		if !strings.Contains(email, "@") || strings.ContainsAny(email, " ,;") {
			problems = append(problems, "no valid email")
		}
		if first, dup := seen[email]; dup && email != "" {
			problems = append(problems, fmt.Sprintf("the same email is on row %d", first))
		}
		seen[email] = row.line
		pp := plannedPerson{name: name, email: email}
		for _, want := range roleSeparator.Split(row.cell(roleCol), -1) {
			if want == "" {
				continue
			}
			at := slices.IndexFunc(roles, func(o option) bool {
				return strings.EqualFold(o.Value, want) || strings.EqualFold(o.Label, want)
			})
			if at < 0 {
				problems = append(problems, fmt.Sprintf("no role called %q (add it under Roles first)", want))
				continue
			}
			if !slices.Contains(pp.roles, roles[at].Value) {
				pp.roles = append(pp.roles, roles[at].Value)
			}
		}
		if len(problems) > 0 {
			iv.add(row.line, label, "problem", capital(strings.Join(problems, "; "))+".")
			continue
		}
		at := slices.IndexFunc(stored, func(p struct {
			ID, Name, Email string
			Active          bool
			Held            []string
		}) bool {
			return p.Email == email
		})
		if at < 0 {
			iv.add(row.line, name, "new", roleNames(roles, pp.roles))
			plan = append(plan, pp)
			continue
		}
		old := stored[at]
		pp.id = old.ID
		var changes []string
		if old.Name != name {
			changes = append(changes, "name was "+old.Name)
		}
		var added []string
		for _, r := range pp.roles {
			if !slices.Contains(old.Held, r) {
				added = append(added, r)
			}
		}
		if len(added) > 0 {
			changes = append(changes, "gets "+roleNames(roles, added))
		}
		if !old.Active {
			changes = append(changes, "stays disabled")
		}
		if len(changes) == 0 || (len(changes) == 1 && !old.Active) {
			iv.add(row.line, name, "unchanged", strings.Join(changes, ""))
			continue
		}
		iv.add(row.line, name, "changed", capital(strings.Join(changes, "; "))+".")
		plan = append(plan, pp)
	}
	return plan, nil
}

func roleNames(roles []option, keys []string) string {
	var names []string
	for _, o := range roles {
		if slices.Contains(keys, o.Value) {
			names = append(names, o.Label)
		}
	}
	return strings.Join(names, ", ")
}

func (s *Server) importPeople(w http.ResponseWriter, r *http.Request, c *call) {
	text, err := sheetText(w, r)
	if err != nil {
		s.settingsProblem(w, r, c, "people", err)
		return
	}
	apply := r.FormValue("action") == "apply"
	var iv *importView
	var links []invite
	err = db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		sh, err := readSheet(text)
		if err != nil {
			return err
		}
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		base := "/t/" + c.tenant.Slug + "/settings/people"
		iv = &importView{Title: "Import people", Back: base, Action: base + "/import", CSV: text}
		plan, err := v.planPeople(sh, iv)
		if err != nil || !apply || iv.Problems > 0 {
			return err
		}
		by := ledger.PersonActor(c.session.PersonID)
		for _, pp := range plan {
			if pp.id == "" {
				if pp.id, err = people.Add(r.Context(), tx, pp.name, pp.email, by); err != nil {
					return err
				}
				code, _, err := auth.NewLink(r.Context(), tx, pp.id, by)
				if err != nil {
					return err
				}
				links = append(links, invite{Name: pp.name, code: code})
			} else if _, _, err := people.Ensure(r.Context(), tx, pp.name, pp.email, by); err != nil {
				return err
			}
			for _, role := range pp.roles {
				if _, err := people.Grant(r.Context(), tx, pp.id, role, by); err != nil {
					return err
				}
			}
		}
		return nil
	})
	switch {
	case err != nil:
		s.settingsProblem(w, r, c, "people", err)
	case apply && iv.Problems == 0:
		notice := imported(iv)
		if len(links) > 0 {
			notice += "\nSend each new person their link below."
		}
		s.showSettings(w, r, c, "people", http.StatusOK, notice, "", s.invites(r, c, links...))
	default:
		s.showImport(w, r, c, "people", iv)
	}
}

// exportPeople downloads everyone who can log in, with their roles, in the shape an import reads.
func (s *Server) exportPeople(w http.ResponseWriter, r *http.Request, c *call) {
	var out bytes.Buffer
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		pv, err := v.people()
		if err != nil {
			return err
		}
		table := [][]string{{"Name", "Email", "Roles"}}
		for _, p := range pv.Rows {
			if p.Active {
				table = append(table, []string{p.Name, p.Email, strings.ReplaceAll(p.RoleNames, ", ", "; ")})
			}
		}
		return writeCSV(&out, table)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sendCSV(w, "people", out.Bytes())
}
