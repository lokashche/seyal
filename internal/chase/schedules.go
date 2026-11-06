// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/start"
)

// findSchedules is the finder for recurring work (D75, R): for each process with a schedule whose run
// is due today, first deciding the run, then starting each entry's item, each its own task (so its own
// short transaction). It only reads; start.Run checks everything again when the task runs.
func findSchedules(ctx context.Context, tx pgx.Tx, b Business) ([]Task, error) {
	plans, err := start.Find(ctx, tx, b.Today)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, p := range plans {
		ref, key := p.PlaybookID+" "+p.Period.Name, "schedule "+p.PlaybookID
		due := p.Period.RunDay
		if p.Problem != "" {
			// It can't run at all (its list lost the yes/no detail): the task fails, so the log says so
			// once a day (see Chaser.business), and Settings shows it.
			problem := errors.New("schedule stopped: " + p.Problem)
			tasks = append(tasks, Task{Ref: ref, Key: key, Due: due, Do: func(context.Context, pgx.Tx) (string, error) {
				return "", problem
			}})
			continue
		}
		if !p.Ready {
			continue
		}
		req := start.Request{PlaybookID: p.PlaybookID, Period: p.Period.Key, Today: b.Today, Inputs: b.inputs()}
		todo := p.Waiting()
		if !p.Decided && !p.HeldBack() {
			tasks = append(tasks, Task{Ref: ref, Key: key, Due: due, Do: run(req)})
			if len(p.Entries) <= p.Schedule.Limit() {
				todo = p.Entries // what the run will most likely cover: start them in the same round
			}
			// Over the limit, the decision holds the run back, so one task per entry would only take the
			// business's lock and do nothing, perhaps thousands of times after a list import.
		}
		for _, e := range todo {
			if p.Started[e.Key] {
				continue
			}
			r := req
			r.Entry = e.Key
			// Just after the decision, so a round decides a run before it starts its items.
			tasks = append(tasks, Task{Ref: ref, Key: key + " " + e.Key, Due: due.Add(time.Nanosecond), Do: run(r)})
		}
	}
	return tasks, nil
}

// run is one step of a run, as a task's action.
func run(r start.Request) func(ctx context.Context, tx pgx.Tx) (string, error) {
	return func(ctx context.Context, tx pgx.Tx) (string, error) { return start.Run(ctx, tx, r) }
}

// inputs are what every event Seyal writes about a schedule adds: the business's day, and the pretend
// day of `dev chase --as-if`.
func (b Business) inputs() map[string]any {
	in := map[string]any{"today": b.Today.Format(dayLayout)}
	if b.AsIf != "" {
		in["as_if"] = b.AsIf
	}
	return in
}
