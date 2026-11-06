// SPDX-License-Identifier: AGPL-3.0-or-later

package start

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"seyal/internal/playbook"
)

// sampleSource is the sample Monthly return's file, with its run day (and, when given, how often it
// runs) changed.
func sampleSource(t *testing.T, version string, runDay int, every string) (string, playbook.Schedule) {
	t.Helper()
	p, err := playbook.LoadFile("../../examples/playbooks/monthly-return.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(p.Source), `version: "0.1"`, `version: "`+version+`"`, 1)
	text = strings.Replace(text, "  day: 1   ", "  day: "+strconv.Itoa(runDay)+"   ", 1)
	if every != "" {
		text = strings.Replace(text, "every: month ", "every: "+every+" ", 1)
	}
	q, err := playbook.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if q.Schedule.Day != runDay || (every != "" && q.Schedule.Every != every) {
		t.Fatalf("the sample's schedule wasn't changed: %+v", q.Schedule)
	}
	return text, *q.Schedule
}

func at(s string) time.Time { return day(s).Add(10 * time.Hour) }

// When a schedule came into use (usesOf), and whether today's run may happen (Ready, without the
// process being on): switching the process off and on again, by Switch on or by choosing a version
// while it is off, never catches up a run whose day passed while it was off; a new version that moves
// the run day or changes the period still does the run the old one hadn't done yet.
func TestUsesOf(t *testing.T) {
	day1, s1 := sampleSource(t, "0.1", 1, "")
	day1b, _ := sampleSource(t, "0.2", 1, "")
	day5, s5 := sampleSource(t, "0.1", 5, "")
	quarter, sq := sampleSource(t, "0.2", 1, "quarter")
	used := func(on, src string) useEvent { return useEvent{Kind: "playbook.used", At: at(on), Source: src} }
	enabled := func(on string) useEvent { return useEvent{Kind: "playbook.enabled", At: at(on)} }
	disabled := func(on string) useEvent { return useEvent{Kind: "playbook.disabled", At: at(on)} }
	for _, c := range []struct {
		name   string
		s      playbook.Schedule
		events []useEvent // newest first
		since  string
		today  string
		ready  bool
	}{
		{"a new version with the same runs keeps the day", s1,
			[]useEvent{used("2026-11-10", day1b), used("2026-09-01", day1)}, "2026-09-01", "2026-11-10", true},
		{"switched on again: no catch-up", s1,
			[]useEvent{enabled("2026-11-10"), disabled("2026-10-20"), used("2026-09-01", day1)}, "2026-11-10", "2026-11-10", false},
		{"a version chosen while off switches it on: no catch-up", s1,
			[]useEvent{used("2026-11-10", day1b), disabled("2026-10-20"), used("2026-09-01", day1)}, "2026-11-10", "2026-11-10", false},
		{"chosen while off, then a later version: still from when it came on", s1,
			[]useEvent{used("2026-11-12", day1), used("2026-11-10", day1b), disabled("2026-10-20"), used("2026-09-01", day1)},
			"2026-11-10", "2026-11-12", false},
		{"switched on again, then the next run happens", s1,
			[]useEvent{used("2026-11-10", day1b), disabled("2026-10-20"), used("2026-09-01", day1)}, "2026-11-10", "2026-12-01", true},
		{"off and on before the run day: the run happens", s1,
			[]useEvent{enabled("2026-10-25"), disabled("2026-10-20"), used("2026-09-01", day1)}, "2026-10-25", "2026-11-01", true},
		{"run day moved from the 5th to the 1st on 3 Nov: October still runs", s1,
			[]useEvent{used("2026-11-03", day1), used("2026-09-01", day5)}, "2026-11-03", "2026-11-03", true},
		{"and November runs on 1 Dec", s1,
			[]useEvent{used("2026-11-03", day1), used("2026-09-01", day5)}, "2026-11-03", "2026-12-01", true},
		{"run day moved after the old run day: October (decided on 5 Nov) may go on", s1,
			[]useEvent{used("2026-11-06", day1), used("2026-09-01", day5)}, "2026-11-06", "2026-11-06", true},
		{"run day moved while off, after the old run day passed: no catch-up", s1,
			[]useEvent{used("2026-11-06", day1), disabled("2026-10-30"), used("2026-09-01", day5)}, "2026-11-06", "2026-11-06", false},
		{"run day moved from the 1st to the 5th: the old September run isn't started again", s5,
			[]useEvent{used("2026-11-03", day5), used("2026-09-01", day1)}, "2026-11-03", "2026-11-03", false},
		{"monthly became quarterly: a new start", sq,
			[]useEvent{used("2026-11-03", quarter), used("2026-09-01", day1)}, "2026-11-03", "2026-11-03", false},
		{"never put in use", s1, nil, "", "2026-11-03", false},
	} {
		u := usesOf(c.events, c.s)
		if got := u.since.Format(time.DateOnly); (c.since == "" && !u.since.IsZero()) || (c.since != "" && got != c.since) {
			t.Errorf("%s: in use since %v, want %s", c.name, u.since, c.since)
		}
		run := Due(c.s, at(c.today), time.UTC, 4)
		ready := cameIntoUse(run, u.since, time.UTC) || u.carries(run, c.s, time.UTC, 4)
		if ready != c.ready {
			t.Errorf("%s: today (%s) the %s run may happen: %v, want %v", c.name, c.today, run.Name, ready, c.ready)
		}
	}
}
