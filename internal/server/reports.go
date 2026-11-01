// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Reports: one work item as a printable page (the browser saves it as PDF), and all items of a
// process as a spreadsheet (CSV). The printable page is the Work Item screen laid out for paper, so
// it is not a new screen; it ends with the item's place in the ledger, so a printed copy can be
// checked against the record later.

type reportView struct {
	Item                 *itemView
	Process, Version     string
	StartedBy, StartedOn string
	Approvals            []reportDecision
	History              []entryView
	LedgerSeq            int64
	LedgerHash           string // the latest ledger event about this item
	FieldsHash           string // what approvals are bound to
	PrintedBy, PrintedOn string
}

type reportDecision struct {
	Name, By, Decision, Reason, When string
}

func (s *Server) report(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	var rv *reportView
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		iv, err := v.item(id)
		if err != nil {
			return err
		}
		it := iv.Item
		p, err := v.playbook(it)
		if err != nil {
			return err
		}
		rv = &reportView{Item: iv, Process: sentence(p.ID), Version: p.Version, FieldsHash: it.FieldsHash,
			StartedBy: v.names[it.CreatedBy], PrintedBy: c.session.Name,
			PrintedOn: time.Now().In(c.tenant.Zone).Format("2 Jan 2006, 15:04")}
		var started time.Time
		if err := tx.QueryRow(r.Context(), `SELECT created_at FROM work_items WHERE tenant_id = current_tenant() AND id::text = $1`,
			id).Scan(&started); err != nil {
			return err
		}
		rv.StartedOn = started.In(c.tenant.Zone).Format("2 Jan 2006")

		// Decisions on the version shown (earlier versions' approvals no longer count).
		rows, _ := tx.Query(r.Context(), `
			SELECT slot, person_id::text, decision, coalesce(reason, ''), decided_at FROM approvals
			WHERE tenant_id = current_tenant() AND work_item_id::text = $1 AND item_version = $2 ORDER BY decided_at`,
			id, it.Version)
		decided, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			Slot, Person, Decision, Reason string
			At                             time.Time
		}])
		if err != nil {
			return err
		}
		for _, d := range decided {
			name := sentence(d.Slot)
			if p.Approvals != nil {
				for _, sl := range p.Approvals.Slots {
					if sl.Slot == d.Slot && v.roles[sl.Role] != "" {
						name = v.roles[sl.Role]
					}
				}
			}
			rv.Approvals = append(rv.Approvals, reportDecision{Name: name, By: v.names[d.Person],
				Decision: map[string]string{engine.Approve: "approved", engine.Reject: "refused"}[d.Decision],
				Reason:   d.Reason, When: d.At.In(c.tenant.Zone).Format("2 Jan 2006, 15:04")})
		}
		for _, e := range iv.Thread {
			if e.Kind == "evt" {
				rv.History = append(rv.History, e)
			}
		}
		var hash []byte
		err = tx.QueryRow(r.Context(), `
			SELECT seq, hash FROM ledger WHERE tenant_id = current_tenant() AND subject_id::text = $1
			ORDER BY seq DESC LIMIT 1`, id).Scan(&rv.LedgerSeq, &hash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		rv.LedgerHash = fmt.Sprintf("%x", hash)
		return nil
	})
	if errors.Is(err, workitem.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "report", c.page(rv))
}

// exportItems downloads every work item of one process (open and closed, all versions) as CSV: the
// item's number, title, status and dates, then one column per detail. Admins only: it is the whole
// record of a process in one file.
func (s *Server) exportItems(w http.ResponseWriter, r *http.Request, c *call) {
	process := r.PathValue("process")
	var out bytes.Buffer
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		rows, _ := tx.Query(r.Context(), `
			SELECT w.id::text, w.created_at, w.updated_at FROM work_items w
			JOIN playbook_versions pv ON pv.tenant_id = w.tenant_id AND pv.id = w.playbook_version_id
			WHERE w.tenant_id = current_tenant() AND pv.playbook_id = $1
			ORDER BY w.created_at, w.number`, process)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			ID               string
			Created, Changed time.Time
		}])
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return errNoTab
		}
		type row struct {
			it               workitem.Item
			p                *playbook.Playbook
			created, changed time.Time
		}
		var items []row
		var columns []playbook.Field // every detail any version has, newest version's order first
		for _, l := range list {
			it, err := workitem.Get(r.Context(), tx, l.ID)
			if err != nil {
				return err
			}
			p, err := v.playbook(it)
			if err != nil {
				return err
			}
			items = append(items, row{it, p, l.Created, l.Changed})
		}
		for i := len(items) - 1; i >= 0; i-- {
			for _, f := range items[i].p.Fields {
				if !slices.ContainsFunc(columns, func(g playbook.Field) bool { return g.Key == f.Key }) {
					columns = append(columns, f)
				}
			}
		}
		table := [][]string{{"Number", "Title", "Status", "With", "Started by", "Started on", "Last changed", "Due"}}
		for _, f := range columns {
			table[0] = append(table[0], ask.Label(f))
		}
		day := func(t time.Time) string { return t.In(c.tenant.Zone).Format(time.DateOnly) }
		for _, x := range items {
			_, status := v.status(x.p, x.it)
			line := []string{x.it.Number, x.it.Title, status, v.holderName(x.it), v.names[x.it.CreatedBy],
				day(x.created), day(x.changed), x.it.DueOn}
			for _, f := range columns {
				if i := slices.IndexFunc(x.p.Fields, func(g playbook.Field) bool { return g.Key == f.Key }); i >= 0 {
					f = x.p.Fields[i] // read the value the way its own version defines it
				}
				line = append(line, v.cell(f, x.it.Fields[f.Key]))
			}
			table = append(table, line)
		}
		return writeCSV(&out, table)
	})
	if errors.Is(err, errNoTab) {
		http.Error(w, "There is no work of this process yet.", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sendCSV(w, process, out.Bytes())
}

// holderName is who has an item, or "" when nobody does (closed work, or not handed to anyone).
func (v *view) holderName(it workitem.Item) string {
	if it.AssigneeID == "" && it.Team == "" {
		return ""
	}
	return strings.TrimSpace(v.holder(it))
}
