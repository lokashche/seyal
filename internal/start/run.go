// SPDX-License-Identifier: AGPL-3.0-or-later

package start

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// How a run goes (D75, R, decision 5):
//   - Each period has one run, on its run day, or at the first round after it until the next run's day.
//   - Its first round decides which list entries it covers, once: one schedule.ran event names them.
//     An entry ticked after that waits for the next run. Too many matches (more than max_per_run) and
//     the run starts none: one schedule.ran with held_back, until the list or the limit changes.
//   - Then each covered entry gets its item, each in its own short transaction (the chaser's 50 a
//     round may spread them over rounds). The item's started_ref, "<process>:<period>:<entry>", has a
//     unique index, so the same work never starts twice, also after a version change or a cancel.
//   - A run before the day its schedule came into use never happens: turning a schedule on never
//     starts past periods, and neither does switching the process off and on again (by Switch on, or
//     by choosing a version while it is off). One exception: when a new version moves the run day or
//     changes the period, a run the old schedule still had to do (its old run day hadn't passed) still
//     happens, so no period is lost (see inUse.carries).

// Plan is what one process's schedule needs in a business on one day. Find works it out by reading
// only; Run does one step of it.
type Plan struct {
	PlaybookID string
	VersionID  string // the version new work starts on
	Playbook   *playbook.Playbook
	Schedule   playbook.Schedule
	Kind       string // the list the schedule goes through (the for_each field's kind)
	Active     bool   // the process is switched on for new work
	Period     Period // the run due today (see Due)
	// Since is when this schedule came into use: the version with these runs (SameRuns) was put in use
	// for new work, or the process was switched on again, whichever is later. Zero: never.
	Since time.Time
	// Ready: the run may happen. The process is on, and the run day isn't before Since (or it is a run
	// the schedule before a change of run day or period still had to do: see inUse.carries).
	Ready bool
	// Problem says why the schedule can't run at all, in plain words ("" when it can), for example a list
	// that no longer has the yes/no detail.
	Problem string
	Decided bool            // the run's entries are decided (a schedule.ran without held_back)
	Held    bool            // a schedule.ran held this run back, and nothing was decided since
	Entries []Entry         // decided: the entries the run covers; otherwise the entries that match now
	Now     int             // how many entries match now (decided or not)
	Started map[string]bool // entry keys that already have an item for this run, open or not

	fiscalStart int
}

// Entry is one list entry a run covers.
type Entry struct {
	Key, Name string
	Active    bool
}

// Waiting returns the entries of a decided run that still need their item: not started, still active.
func (p Plan) Waiting() []Entry {
	if !p.Decided {
		return nil
	}
	var out []Entry
	for _, e := range p.Entries {
		if e.Active && !p.Started[e.Key] {
			out = append(out, e)
		}
	}
	return out
}

// Next is the run after today's.
func (p Plan) Next() Period { return Next(p.Schedule, p.Period, p.fiscalStart) }

// HeldBack reports whether the run waits because too many entries match: held back before, and still
// over the limit.
func (p Plan) HeldBack() bool {
	return !p.Decided && p.Held && len(p.Entries) > p.Schedule.Limit()
}

// Ref is the started_ref of the run's item for one entry: "monthly-return:m2026-11:sundaram".
func (p Plan) Ref(entry string) string { return p.prefix() + entry }

func (p Plan) prefix() string { return p.PlaybookID + ":" + p.Period.Key + ":" }

// Find returns the plan of every process the business has switched on whose current version has a
// schedule, for the business's day today. It only reads. tx must come from db.WithTenant.
func Find(ctx context.Context, tx pgx.Tx, today time.Time) ([]Plan, error) {
	ids, err := scheduled(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out []Plan
	for _, id := range ids {
		p, ok, err := For(ctx, tx, id, today)
		if err != nil {
			return nil, fmt.Errorf("schedule of %s: %w", id, err)
		}
		if ok && p.Active {
			out = append(out, p)
		}
	}
	return out, nil
}

// scheduled lists the processes switched on, sorted. For reads each one's current version and skips
// those without a schedule (a business has a handful of processes).
func scheduled(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, _ := tx.Query(ctx, `
		SELECT playbook_id FROM tenant_playbooks WHERE tenant_id = current_tenant() AND active ORDER BY playbook_id`)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// For works out the plan of one process for the business's day today, whether it is switched on or
// not (Settings shows it either way). ok is false when the process isn't the business's or its current
// version has no schedule. It only reads. tx must come from db.WithTenant.
func For(ctx context.Context, tx pgx.Tx, playbookID string, today time.Time) (p Plan, ok bool, err error) {
	var source, zone string
	var fiscalStart int
	err = tx.QueryRow(ctx, `
		SELECT t.active, v.id::text, v.source, b.timezone, b.fiscal_year_start_month
		FROM tenant_playbooks t
		JOIN playbook_versions v ON v.tenant_id = t.tenant_id AND v.id = t.current_version_id
		JOIN tenants b ON b.id = t.tenant_id
		WHERE t.tenant_id = current_tenant() AND t.playbook_id = $1`, playbookID).
		Scan(&p.Active, &p.VersionID, &source, &zone, &fiscalStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, err
	}
	if p.Playbook, err = playbook.Parse([]byte(source)); err != nil {
		return Plan{}, false, err
	}
	if p.Playbook.Schedule == nil {
		return Plan{}, false, nil
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return Plan{}, false, fmt.Errorf("time zone %q: %w", zone, err)
	}
	p.PlaybookID, p.Schedule, p.fiscalStart = playbookID, *p.Playbook.Schedule, fiscalStart
	p.Period = Due(p.Schedule, today, loc, fiscalStart)
	u, err := inUseSince(ctx, tx, playbookID, p.Schedule)
	if err != nil {
		return Plan{}, false, err
	}
	p.Since = u.since
	p.Ready = p.Active && (cameIntoUse(p.Period, p.Since, loc) || u.carries(p.Period, p.Schedule, loc, fiscalStart))
	if p.Kind, p.Problem, err = listProblem(ctx, tx, p.Playbook); err != nil || p.Problem != "" {
		return p, true, err
	}
	if err := p.readRun(ctx, tx); err != nil {
		return Plan{}, false, err
	}
	return p, true, nil
}

// cameIntoUse reports whether run's day is on or after the day (in zone) its schedule came into use.
// A schedule that never came into use (since is zero) has no runs.
func cameIntoUse(run Period, since time.Time, zone *time.Location) bool {
	if since.IsZero() {
		return false
	}
	y, m, d := since.In(zone).Date()
	return !run.RunDay.Before(time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
}

// inUseSince reads what the ledger says about when schedule s of the process came into use (see
// usesOf).
func inUseSince(ctx context.Context, tx pgx.Tx, playbookID string, s playbook.Schedule) (inUse, error) {
	rows, _ := tx.Query(ctx, `
		SELECT l.kind, l.occurred_at, coalesce(v.source, '')
		FROM ledger l
		LEFT JOIN playbook_versions v ON v.tenant_id = l.tenant_id AND v.id = l.subject_id
		WHERE l.tenant_id = current_tenant() AND l.kind IN ('playbook.used', 'playbook.enabled', 'playbook.disabled')
			AND l.body::jsonb #>> '{payload,playbook}' = $1
		ORDER BY l.seq DESC`, playbookID)
	events, err := pgx.CollectRows(rows, pgx.RowToStructByPos[useEvent])
	if err != nil {
		return inUse{}, fmt.Errorf("when the schedule came into use: %w", err)
	}
	return usesOf(events, s), nil
}

// useEvent is one ledger event about a process being put in use or switched on or off:
// playbook.used (with the version's source), playbook.enabled or playbook.disabled.
type useEvent struct {
	Kind   string
	At     time.Time
	Source string
}

// inUse is when a schedule came into use, as usesOf works it out.
type inUse struct {
	since time.Time // Plan.Since
	// on is when the process was last switched on again after a switch-off: by Switch on
	// (playbook.enabled), or by choosing a version while it was off (playbook.used). Zero: never.
	on time.Time
	// When the runs changed, before is the schedule in use just before (in use since beforeSince), and
	// changed is when the new runs came into use. nil: the runs never changed, or there were none before.
	before               *playbook.Schedule
	changed, beforeSince time.Time
}

// usesOf works out from the events (newest first) when schedule s came into use: the oldest of the
// latest "put in use" events (playbook.used) whose versions all have the same runs as s, or the time the
// process was last switched on again, whichever is later. A version bump that keeps the runs keeps the
// day. Switching the process off and on starts from the moment it came on again, so a run whose day
// passed while it was off isn't caught up. It comes on again by Switch on (playbook.enabled), and also
// by choosing a version while it is off (playbook.Use switches it on, and writes only playbook.used).
func usesOf(events []useEvent, s playbook.Schedule) inUse {
	var u inUse
	var anchor, enabled, onAfterOff time.Time
	off := false // a playbook.disabled was passed (the walk goes back in time)
	stage := 0   // 0: versions with s's runs; 1: the version before, with other runs; 2: done
	for _, e := range events {
		if e.Kind == "playbook.disabled" {
			off = true
			continue
		}
		if !off {
			onAfterOff = e.At // in the end: the oldest switch-on event after the latest switch-off
		}
		if e.Kind == "playbook.enabled" {
			if enabled.IsZero() {
				enabled = e.At
			}
			continue
		}
		if stage == 2 {
			continue
		}
		var ps *playbook.Schedule
		if p, err := playbook.Parse([]byte(e.Source)); err == nil {
			ps = p.Schedule
		}
		switch {
		case stage == 0 && ps != nil && ps.SameRuns(s):
			anchor = e.At
		case stage == 0 && ps != nil && !anchor.IsZero():
			// The runs changed at anchor: remember the schedule before, for the runs it still had to do.
			stage, u.before, u.changed, u.beforeSince = 1, ps, anchor, e.At
		case stage == 1 && ps != nil && ps.SameRuns(*u.before):
			u.beforeSince = e.At
		default:
			stage = 2 // a version before had no schedule, or other runs again
		}
	}
	u.on = enabled
	if off {
		u.on = onAfterOff
	}
	if anchor.IsZero() {
		return inUse{}
	}
	u.since = anchor
	if u.on.After(anchor) {
		u.since = u.on
	}
	return u
}

// carries reports whether run, under schedule s, is a run that the schedule in use before a change of
// runs still had to do: the old schedule's run of the same period was due when the runs changed, or came
// after. Moving the run day from the 5th to the 1st on 3 Nov then still starts October's work (its old
// run day was 5 Nov) instead of losing it. It only applies when the change keeps how often it runs and
// the list (every, for_each), so the same period means the same work. The started_ref and the
// schedule.ran key have no version in them, so nothing starts twice.
func (u inUse) carries(run Period, s playbook.Schedule, zone *time.Location, fiscalStart int) bool {
	b := u.before
	if b == nil || b.Every != s.Every || b.ForEach != s.ForEach {
		return false
	}
	was := Due(*b, u.changed, zone, fiscalStart)
	for _, c := range []Period{was, Next(*b, was, fiscalStart)} {
		if c.Key == run.Key {
			// Under the old schedule this run would have happened: its old day is on or after the day the
			// old schedule came into use, and the day the process was last switched on.
			return cameIntoUse(c, u.beforeSince, zone) && (u.on.IsZero() || cameIntoUse(c, u.on, zone))
		}
	}
	return false
}

// listProblem checks that the schedule's list still has what it needs: the list, and its yes/no
// detail. A version is checked when it is added, and a list change that removes the detail is refused,
// so this is only a guard: the round skips the schedule, and Settings says why.
func listProblem(ctx context.Context, tx pgx.Tx, p *playbook.Playbook) (kind, problem string, err error) {
	s := p.Schedule
	i := slices.IndexFunc(p.Fields, func(f playbook.Field) bool { return f.Key == s.ForEach })
	if i < 0 || p.Fields[i].Type != "master" {
		return "", fmt.Sprintf("the schedule's for_each %q isn't a list field of the process", s.ForEach), nil
	}
	kind = p.Fields[i].Kind
	mk, err := playbook.Kind(ctx, tx, kind)
	if err != nil || mk == nil {
		return kind, fmt.Sprintf("the business has no %q list", kind), err
	}
	j := slices.IndexFunc(mk.Fields, func(f playbook.Field) bool { return f.Key == s.Where })
	if j < 0 || mk.Fields[j].Type != "bool" {
		return kind, fmt.Sprintf("the %s list has no yes/no detail %q", mk.Label, s.Where), nil
	}
	return kind, "", nil
}

// ranEvent is the part of a schedule.ran event that readRun reads back. The event also has the
// process's version, the period's name, the run day, the count, the limit and the inputs (see decide).
type ranEvent struct {
	Entries  []string `json:"entries"`
	HeldBack bool     `json:"held_back"`
}

// readRun reads what the ledger says about this run (decided or held back), the entries it covers or
// would cover now, and which of them already have their item.
func (p *Plan) readRun(ctx context.Context, tx pgx.Tx) error {
	rows, _ := tx.Query(ctx, `
		SELECT body::jsonb -> 'payload' FROM ledger
		WHERE tenant_id = current_tenant() AND kind = 'schedule.ran'
			AND body::jsonb #>> '{payload,playbook}' = $1 AND body::jsonb #>> '{payload,period}' = $2
		ORDER BY seq`, p.PlaybookID, p.Period.Key)
	events, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return fmt.Errorf("runs: %w", err)
	}
	var keys []string
	for _, raw := range events {
		var e ranEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("runs: %w", err)
		}
		if e.HeldBack {
			p.Held = true
			continue
		}
		p.Decided, keys = true, e.Entries
		break // the first decision is the run's: it is never decided again
	}
	now, err := matching(ctx, tx, p.Kind, p.Schedule.Where)
	if err != nil {
		return err
	}
	p.Entries, p.Now = now, len(now)
	if p.Decided {
		if p.Entries, err = entries(ctx, tx, p.Kind, keys); err != nil {
			return err
		}
	}
	rows, _ = tx.Query(ctx, `
		SELECT substr(started_ref, length($1) + 1) FROM work_items
		WHERE tenant_id = current_tenant() AND started_via = 'schedule' AND left(started_ref, length($1)) = $1`, p.prefix())
	started, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("started items: %w", err)
	}
	p.Started = map[string]bool{}
	for _, k := range started {
		p.Started[k] = true
	}
	return nil
}

// matching lists the active entries of a list that have the yes/no detail where ticked, by key.
func matching(ctx context.Context, tx pgx.Tx, kind, where string) ([]Entry, error) {
	rows, _ := tx.Query(ctx, `
		SELECT key, name, active FROM master_items
		WHERE tenant_id = current_tenant() AND kind = $1 AND active AND fields -> $2 = 'true'::jsonb
		ORDER BY key`, kind, where)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Entry])
	if err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	return out, nil
}

// entries returns the decided entries of a run, in its order; one no longer in the list counts as not
// active.
func entries(ctx context.Context, tx pgx.Tx, kind string, keys []string) ([]Entry, error) {
	rows, _ := tx.Query(ctx, `
		SELECT key, name, active FROM master_items WHERE tenant_id = current_tenant() AND kind = $1 AND key = ANY($2)`,
		kind, keys)
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Entry])
	if err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	byKey := map[string]Entry{}
	for _, e := range found {
		byKey[e.Key] = e
	}
	out := make([]Entry, 0, len(keys))
	for _, k := range keys {
		e, ok := byKey[k]
		if !ok {
			e = Entry{Key: k, Name: k}
		}
		out = append(out, e)
	}
	return out, nil
}

// A Request is one step of a run, for the chaser: decide the run (Entry ""), or start one entry's item.
type Request struct {
	PlaybookID string
	Period     string    // the run's period key, as found (if today's run is another one now, nothing happens)
	Entry      string    // "" decides the run; otherwise the entry whose item to start
	Today      time.Time // the business's day (with `dev chase --as-if`, the pretend one)
	// Inputs go into the events, so the ledger shows later what Seyal went by: today, and as_if.
	Inputs map[string]any
}

// Run does one step of a run, as Seyal (system): it reads the plan again (the caller holds the
// business's ledger lock, so nothing changes meanwhile), checks the step is still due, and does it. It
// says what it did in a few words for the log, never naming a person, or "" when nothing was due. tx
// must come from db.WithTenant.
func Run(ctx context.Context, tx pgx.Tx, r Request) (string, error) {
	p, ok, err := For(ctx, tx, r.PlaybookID, r.Today)
	switch {
	case err != nil:
		return "", err
	case !ok || !p.Ready || p.Period.Key != r.Period:
		return "", nil // switched off, a new version, or another day's run now
	case p.Problem != "":
		return "", errors.New("schedule stopped: " + p.Problem)
	case r.Entry == "":
		return decide(ctx, tx, p, r)
	}
	return startOne(ctx, tx, p, r)
}

// decide writes the run's schedule.ran: the entries it covers, or held_back when there are too many.
func decide(ctx context.Context, tx pgx.Tx, p Plan, r Request) (string, error) {
	if p.Decided || p.HeldBack() {
		return "", nil // decided once; or held back already and still too many (logged once)
	}
	count, limit := len(p.Entries), p.Schedule.Limit()
	payload := map[string]any{"playbook": p.PlaybookID, "version": p.Playbook.Version, "period": p.Period.Key,
		"period_name": p.Period.Name, "run_day": p.Period.RunDay.Format(time.DateOnly), "count": count, "limit": limit}
	maps.Copy(payload, r.Inputs)
	did := fmt.Sprintf("the %s run covers %d", p.Period.Name, count)
	if count > limit {
		payload["held_back"] = true
		did = fmt.Sprintf("the %s run is held back: %d match, the limit is %d", p.Period.Name, count, limit)
	} else {
		keys := []string{}
		for _, x := range p.Entries {
			keys = append(keys, x.Key)
		}
		payload["entries"] = keys
	}
	_, err := ledger.Append(ctx, tx, ledger.Event{Kind: "schedule.ran", SubjectType: "playbook_version",
		SubjectID: p.VersionID, Payload: payload}.By(ledger.SystemActor))
	if err != nil {
		return "", err
	}
	return did, nil
}

// startOne starts the item of one entry of a decided run, under the version new work starts on.
func startOne(ctx context.Context, tx pgx.Tx, p Plan, r Request) (string, error) {
	i := slices.IndexFunc(p.Waiting(), func(e Entry) bool { return e.Key == r.Entry })
	if i < 0 {
		return "", nil // not in the run, no longer active, or started already
	}
	e := p.Waiting()[i]
	s := p.Schedule
	fields := engine.Fields{s.ForEach: e.Key}
	if s.PeriodField != "" {
		fields[s.PeriodField] = p.Period.Name
	}
	if s.Due.Field != "" {
		fields[s.Due.Field] = p.Period.DueOn
	}
	extra := map[string]any{"period_key": p.Period.Key}
	maps.Copy(extra, r.Inputs)
	// A savepoint: if another copy of the app started the same item first, the unique started_ref
	// refuses this insert, and only this savepoint is undone (the transaction stays usable).
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer sp.Rollback(ctx) // nothing to undo once committed
	it, err := workitem.Start(ctx, sp, workitem.NewItem{PlaybookVersionID: p.VersionID, Via: "schedule",
		Ref: p.Ref(e.Key), Fields: fields, By: ledger.SystemActor, Period: p.Period.Name,
		Reason: fmt.Sprintf("%s schedule: %s, for %s", every(s.Every), p.Period.Name, e.Name), Extra: extra})
	if repeat(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := sp.Commit(ctx); err != nil {
		return "", err
	}
	return fmt.Sprintf("started %s (the %s run)", it.Number, p.Period.Name), nil
}

// repeat reports whether err is the unique started_ref refusing a second start of the same work.
func repeat(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && strings.Contains(pg.ConstraintName, "schedule")
}

// every is how a schedule is named: "Monthly", "Quarterly", "Yearly".
func every(word string) string {
	switch word {
	case "quarter":
		return "Quarterly"
	case "year":
		return "Yearly"
	}
	return "Monthly"
}

// Every names how often a schedule runs, in small letters: "monthly", "quarterly", "yearly".
func Every(word string) string { return strings.ToLower(every(word)) }
