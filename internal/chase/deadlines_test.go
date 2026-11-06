// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"testing"
	"time"

	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// TestWorkingDaysAfter is the design's table of how a wait is counted (D75, decision 1): working days
// after the day the stay started, without weekly days off and holidays. 2 Nov 2026 is a Monday.
func TestWorkingDaysAfter(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	sunday := []time.Weekday{time.Sunday}
	for _, tc := range []struct {
		name     string
		start    string
		n        int
		daysOff  []time.Weekday
		holidays []string
		want     string
	}{
		{"Mon + 2 days: Wed", "2026-11-02", 2, nil, nil, "2026-11-04"},
		{"Fri + 2 days, Sunday off: Sat counts, Sun doesn't, so Mon", "2026-11-06", 2, sunday, nil, "2026-11-09"},
		{"Fri + 2 days, no days off: Sun", "2026-11-06", 2, nil, nil, "2026-11-08"},
		{"started on a Sunday: Mon is day 1", "2026-11-08", 1, sunday, nil, "2026-11-09"},
		{"Fri + 2 days, Sunday off and Mon 9 Nov a holiday: Sat, then Tue", "2026-11-06", 2, sunday, []string{"2026-11-09"}, "2026-11-10"},
		{"the story: Mon 30 Nov + 4 days, Sunday off: Fri 4 Dec", "2026-11-30", 4, sunday, nil, "2026-12-04"},
		{"the gap after a reminder on Fri 11 Dec, Sunday off: Mon 14 Dec", "2026-12-11", 2, sunday, nil, "2026-12-14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workingDaysAfter(day(tc.start), tc.n, tc.daysOff, tc.holidays)
			if got.Format(time.DateOnly) != tc.want {
				t.Errorf("workingDaysAfter(%s, %d) = %s, want %s", tc.start, tc.n, got.Format(time.DateOnly), tc.want)
			}
			// Counting back gives the same number: on that day the stay has waited n working days.
			cal := Calendar{Zone: time.UTC, DaysOff: tc.daysOff, Holidays: tc.holidays}
			if n := cal.WorkingDays(day(tc.start), got); n != tc.n {
				t.Errorf("WorkingDays(%s, %s) = %d, want %d", tc.start, tc.want, n, tc.n)
			}
			if n := cal.WorkingDays(day(tc.start), got.AddDate(0, 0, -1)); n >= tc.n {
				t.Errorf("the day before %s already counts %d working days, want fewer than %d", tc.want, n, tc.n)
			}
		})
	}
}

// TestDaysInTheBusinessZone: days are dates in the business's time zone. 19:00 UTC on Mon 2 Nov is
// already Tue 3 Nov in Kolkata, so a stay that started then has waited 1 working day on Wed 4 Nov,
// not 2.
func TestDaysInTheBusinessZone(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	cal := Calendar{Zone: kolkata}
	start := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	wed := time.Date(2026, 11, 4, 0, 30, 0, 0, kolkata)
	if n := cal.WorkingDays(start, wed); n != 1 {
		t.Errorf("WorkingDays = %d, want 1 (the stay started on Tue 3 Nov in Kolkata)", n)
	}
	if n := (Calendar{Zone: time.UTC}).WorkingDays(start, wed); n != 1 {
		t.Errorf("in UTC: WorkingDays = %d, want 1 (Wed 00:30 in Kolkata is still Tue in UTC)", n)
	}
	holidays := Calendar{Zone: kolkata, Holidays: []string{"2026-11-03", "2026-11-04", "2026-11-20"}}
	if got := holidays.HolidaysIn(start, wed); len(got) != 1 || got[0] != "2026-11-04" {
		t.Errorf("HolidaysIn = %v, want [2026-11-04] (the start day itself never counts)", got)
	}
}

// timed is a step with the sample's timing: remind after 2 working days, escalate after 4.
var timed = &playbook.Playbook{
	States: []string{"open", "waiting", "done"},
	Steps: map[string]playbook.Step{
		"open":    {Owner: "role:ops", RemindAfter: "2 days", EscalateAfter: "4 days", EscalateTo: "role:owner"},
		"waiting": {RemindAfter: "1 day"},
	},
}

// stay follows one item through its step stay day by day (every day a working day), as the chaser
// would, starting on day 0 at 10:00 with Ben. Each day, it does what is due once (one round), and
// records the moments in the snapshot's form.
type stay struct {
	t     *testing.T
	start time.Time
	it    workitem.Item
	log   []string // "day 2: remind"
}

func newStay(t *testing.T, state string) *stay {
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	s := &stay{t: t, start: start}
	s.it = workitem.Item{State: state, AssigneeID: "ben", StepSince: stamp(start), HeldSince: stamp(start.Add(time.Minute))}
	return s
}

func stamp(t time.Time) string { return t.Format(workitem.HeldSinceLayout) }

// at is a moment on day n.
func (s *stay) at(n, hour int) time.Time {
	return s.start.AddDate(0, 0, n).Add(time.Duration(hour-10) * time.Hour)
}

// pass gives the item to someone else on day n at 15:00.
func (s *stay) pass(n int, to string) {
	s.it.AssigneeID, s.it.HeldSince = to, stamp(s.at(n, 15))
}

// round runs one round on day n (just after midnight, 00:05) and returns what it did.
func (s *stay) round(n int) string {
	what, _ := due(timed, s.it, Calendar{Zone: time.UTC}, midnight(s.at(n, 0)))
	now := stamp(s.at(n, 0).Add(5 * time.Minute))
	switch what {
	case remindDue:
		s.it.RemindedAt, s.it.Reminders = now, s.it.Reminders+1
	case escalateDue:
		s.it.EscalatedAt, s.it.HeldSince, s.it.AssigneeID, s.it.Team = now, now, "", "owner"
	}
	return what
}

// days runs one round on each day from first to last, and returns the days something happened.
func (s *stay) days(first, last int) map[int]string {
	out := map[int]string{}
	for n := first; n <= last; n++ {
		if what := s.round(n); what != "" {
			out[n] = what
		}
	}
	return out
}

func wantDays(t *testing.T, what string, got, want map[int]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %v, want %v", what, got, want)
		return
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s: %v, want %v", what, got, want)
			return
		}
	}
}

// TestDueStory: a stay with one holder is reminded on day 2 and escalated on day 4; then nothing more
// happens in that stay. Running the same day twice does nothing the second time.
func TestDueStory(t *testing.T) {
	s := newStay(t, "open")
	wantDays(t, "one holder", s.days(0, 10), map[int]string{2: remindDue, 4: escalateDue})
	s = newStay(t, "open")
	if s.round(2) != remindDue || s.round(2) != "" {
		t.Error("day 2, two rounds: want one reminder, then nothing")
	}
}

// TestDuePassedAround: passed A→B→A every 2 days, the stay still escalates by day 6 (E + gap): Ben is
// reminded on day 2 and passes it to Dev; Dev gets the one new-holder reminder on day 4 and passes it
// back; on day 6 it is escalated, though Ben wasn't reminded after he got it back.
func TestDuePassedAround(t *testing.T) {
	s := newStay(t, "open")
	got := map[int]string{}
	for n := 0; n <= 10; n++ {
		if what := s.round(n); what != "" {
			got[n] = what
		}
		if n%2 == 0 && n > 0 && s.it.EscalatedAt == "" {
			s.pass(n, map[string]string{"ben": "dev", "dev": "ben"}[s.it.AssigneeID])
		}
	}
	wantDays(t, "passed every 2 days", got, map[int]string{2: remindDue, 4: remindDue, 6: escalateDue})
	if s.it.Reminders != 2 {
		t.Errorf("reminders = %d, want 2 (at most two in a stay)", s.it.Reminders)
	}
}

// TestDueNewHolder: the design's example. Ben is reminded on day 2 and passes it to Dev on day 3; on
// day 4 Dev, never warned, gets the new-holder reminder; the escalation waits the gap (day 6).
// Passing it back to Ben on day 5 changes nothing: no third reminder.
func TestDueNewHolder(t *testing.T) {
	s := newStay(t, "open")
	got := s.days(0, 3)
	s.pass(3, "dev")
	for n, w := range s.days(4, 4) {
		got[n] = w
	}
	s.pass(5, "ben")
	for n, w := range s.days(5, 10) {
		got[n] = w
	}
	wantDays(t, "new holder", got, map[int]string{2: remindDue, 4: remindDue, 6: escalateDue})

	// Taken from a team that was reminded: the taker is a new holder.
	s = newStay(t, "open")
	s.it.AssigneeID, s.it.Team, s.it.HeldSince = "", "ops", s.it.StepSince
	got = s.days(0, 2)
	s.it.AssigneeID, s.it.Team, s.it.HeldSince = "dev", "", stamp(s.at(3, 9))
	for n, w := range s.days(3, 10) {
		got[n] = w
	}
	wantDays(t, "taken from a reminded team", got, map[int]string{2: remindDue, 4: remindDue, 6: escalateDue})
}

// TestDueCatchUp: the server was off from day 1 to day 5 (decision 3). On day 5 both times have passed:
// Seyal reminds first, and escalates only once the full gap has passed after that reminder (day 7),
// not in the next round.
func TestDueCatchUp(t *testing.T) {
	s := newStay(t, "open")
	got := s.days(0, 0)
	for n, w := range s.days(5, 10) {
		got[n] = w
	}
	if s.round(7) != "" {
		t.Error("a second round on the day of the escalation did something")
	}
	wantDays(t, "after downtime", got, map[int]string{5: remindDue, 7: escalateDue})
}

// TestDueOtherCases: a step that only reminds does so once; work nobody has counts as warned once its
// reminder is written; an escalated stay, a stay with no start, and a step without timing get nothing.
func TestDueOtherCases(t *testing.T) {
	s := newStay(t, "waiting")
	wantDays(t, "remind only", s.days(0, 10), map[int]string{1: remindDue})

	s = newStay(t, "open")
	s.it.AssigneeID, s.it.HeldSince = "", ""
	wantDays(t, "nobody has it", s.days(0, 10), map[int]string{2: remindDue, 4: escalateDue})

	s = newStay(t, "open")
	s.it.EscalatedAt = stamp(s.at(1, 0))
	wantDays(t, "already escalated", s.days(0, 10), map[int]string{})

	s = newStay(t, "open")
	s.it.StepSince = ""
	wantDays(t, "no step start", s.days(0, 10), map[int]string{})

	s = newStay(t, "done")
	wantDays(t, "no timing", s.days(0, 10), map[int]string{})
}
