// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Deadlines that act (D75, D77; Phase 2, M6). A step's timing in the process file says when Seyal
// reminds whoever must act (remind_after) and when it hands the item on (escalate_after, to
// escalate_to). With R for remind_after, E for escalate_after and the gap E − R, for each step stay:
//
//   - the first reminder is due once the stay has waited R working days;
//   - the escalation is due once the stay has waited E working days, at least the gap has passed since
//     the last reminder, and whoever has it now was reminded after they got it (or the stay already had
//     its one reminder for a new holder). Otherwise a new holder who was never warned gets that one
//     reminder now, and the escalation waits the gap.
//
// So each stay has at most two reminders, then one escalation; passing the item around can't put it
// off past day E + gap. After an escalation, chasing for the stay is over.

// What is due for an item now.
const (
	nothingDue  = ""
	remindDue   = "remind"
	escalateDue = "escalate"
)

// due works out what is due for an item in its step today, and how many working days its stay has
// waited. It only reads the item: what was done is kept on it (D76).
func due(p *playbook.Playbook, it workitem.Item, cal Calendar, today time.Time) (string, int) {
	remind, escalate := p.Steps[it.State].Timing()
	since, err := time.Parse(workitem.HeldSinceLayout, it.StepSince)
	if remind == 0 || err != nil || it.EscalatedAt != "" {
		return nothingDue, 0
	}
	days := cal.WorkingDays(since, today)
	switch {
	case it.Reminders == 0 && days >= remind:
		return remindDue, days
	case it.Reminders == 0, escalate == 0, days < escalate:
		return nothingDue, days // too early, or a step that only reminds (once)
	}
	// Whoever has it now was warned: they got a reminder after they got the item. Work nobody has
	// counts as warned once its reminder is written.
	warned := it.RemindedAt >= it.HeldSince || (it.AssigneeID == "" && it.Team == "")
	if !warned && it.Reminders < 2 {
		return remindDue, days // the one reminder for a new holder
	}
	reminded, err := time.Parse(workitem.HeldSinceLayout, it.RemindedAt)
	if err != nil || cal.WorkingDays(reminded, today) < escalate-remind {
		return nothingDue, days // wait the full gap after the last reminder
	}
	return escalateDue, days
}

// findDeadlines is the finder for deadlines that act: the open items whose step has timing and for
// which a reminder or an escalation is due today. It only reads.
func findDeadlines(ctx context.Context, tx pgx.Tx, b Business) ([]Task, error) {
	items, books, err := workitem.Timed(ctx, tx)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, it := range items {
		if it.StepSince == "" {
			// Every way into a step sets step_since (D76), so this is a bug: say so once a day.
			tasks = append(tasks, Task{Ref: it.Number, Key: it.ID, Do: func(context.Context, pgx.Tx) (string, error) {
				return "", errNoStepSince
			}})
			continue
		}
		p := books[it.PlaybookVersionID]
		what, _ := due(p, it, b.Calendar, b.Today)
		if what == nothingDue {
			continue
		}
		if a := p.Approvals; what == remindDue && a != nil && it.State == a.In {
			// No approval missing: nobody to remind, and nothing would mark the stay, so without this it
			// would be found (and lock the ledger) every round. An error: the task fails the same way and
			// waits until tomorrow.
			if nobody, err := workitem.NobodyToRemind(ctx, tx, it); err == nil && nobody {
				continue
			}
		}
		since, _ := time.Parse(workitem.HeldSinceLayout, it.StepSince)
		tasks = append(tasks, Task{Ref: it.Number, Key: it.ID, Due: since, Do: b.chaseItem(it.ID, it.StepSince)})
	}
	return tasks, nil
}

var errNoStepSince = errors.New("it is in a step with timing but has no step start (step_since), so it is never chased; please report this")

// chaseItem is one item's action: read it again (the ledger lock is held, so nobody else changes it
// meanwhile), check it is in the same step stay and still due, then remind or escalate it as Seyal.
func (b Business) chaseItem(id, stepSince string) func(ctx context.Context, tx pgx.Tx) (string, error) {
	return func(ctx context.Context, tx pgx.Tx) (string, error) {
		it, err := workitem.Get(ctx, tx, id)
		if err != nil || it.StepSince != stepSince {
			return "", err // it moved to another step since it was found
		}
		p, err := workitem.PlaybookFor(ctx, tx, it)
		if err != nil {
			return "", err
		}
		what, days := due(p, it, b.Calendar, b.Today)
		if what == nothingDue {
			return "", nil
		}
		st := p.Steps[it.State]
		since, _ := time.Parse(workitem.HeldSinceLayout, it.StepSince)
		inputs := map[string]any{
			"today":        b.Today.Format(dayLayout),
			"days_off":     dayNames(b.Calendar.DaysOff),
			"holidays":     b.Calendar.HolidaysIn(since, b.Today),
			"step_since":   it.StepSince,
			"remind_after": st.RemindAfter,
		}
		if st.EscalateAfter != "" {
			inputs["escalate_after"] = st.EscalateAfter
		}
		if b.AsIf != "" {
			inputs["as_if"] = b.AsIf
		}
		c := workitem.Chase{StepSince: it.StepSince, Reminders: it.Reminders, Days: days, Now: b.Now,
			Zone: b.Zone, Inputs: inputs}
		if what == escalateDue {
			return workitem.Escalate(ctx, tx, id, c)
		}
		return workitem.Remind(ctx, tx, id, c)
	}
}

// dayNames writes days off as the set-up file does: [sunday].
func dayNames(days []time.Weekday) []string {
	out := []string{}
	for _, d := range days {
		out = append(out, [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}[d])
	}
	return out
}
