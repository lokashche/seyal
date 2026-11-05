// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/db"
)

// Settings → AI: which model each kind of AI work uses, and what AI has cost, so models can be
// chosen from real numbers.

type aiView struct {
	Purposes                            []aiPurpose
	Days                                int
	Rate                                float64 // rupees per US dollar used for the estimates
	Total                               aiLine
	ByPurpose, ByModel, ByPerson, ByDay []aiLine
}

type aiPurpose struct {
	Name, What, Model, Price string
	On                       bool
}

type aiLine struct {
	Label, Calls, Failed, Input, Cached, Output, USD, INR string
}

var purposeWhat = map[ai.Purpose]string{
	ai.Understand: "Reads chat messages: what they are and which details they give.",
	ai.Draft:      "Drafts process definitions from a plain description.",
	ai.Tidy:       "Tidies the words of voice notes (removes \"umm\"s and repeats) before the person checks them.",
	ai.Summarize:  "Summaries and reports.",
	ai.Transcribe: "Turns voice notes into words, which the person corrects before sending.",
}

func (s *Server) aiPage(w http.ResponseWriter, r *http.Request, c *call) {
	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 366 {
		days = d
	}
	av := aiView{Days: days, Rate: s.ai.USDtoINR}
	for _, p := range ai.Purposes {
		ap := aiPurpose{Name: sentence(string(p)), What: purposeWhat[p], Model: "off"}
		if choice, on := s.ai.For(p); on {
			ap.On, ap.Model = true, choice.Name
			pr := choice.Price
			ap.Price = fmt.Sprintf("$%g in · $%g cached · $%g cache write · $%g out (per million tokens)",
				pr.Input, pr.Cached, pr.CacheWrite, pr.Output)
			if pr == (ai.Price{}) {
				ap.Price = "no price set: costs show as 0"
			}
		}
		av.Purposes = append(av.Purposes, ap)
	}
	sp := aiPurpose{Name: sentence(string(ai.Transcribe)), What: purposeWhat[ai.Transcribe], Model: "off"}
	if speech, on := s.ai.Speech(); on {
		sp.On, sp.Model = true, speech.Name
		sp.Price = fmt.Sprintf("$%g per hour of audio", speech.PerHour)
		if speech.PerHour == 0 {
			sp.Price = "no price set: costs show as 0"
		}
		if speech.Language != "" {
			sp.Model += " · language " + speech.Language
		}
	}
	av.Purposes = append(av.Purposes, sp)

	var rep ai.Report
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		var err error
		rep, err = ai.MakeReport(r.Context(), tx, time.Now().AddDate(0, 0, -days), c.tenant.Zone)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	line := func(l ai.Line) aiLine {
		return aiLine{Label: l.Label, Calls: strconv.Itoa(l.Calls), Failed: strconv.Itoa(l.Failed),
			Input: count(l.Input), Cached: count(l.Cached), Output: count(l.Output),
			USD: fmt.Sprintf("$%.2f", l.CostUSD), INR: money(l.CostUSD*s.ai.USDtoINR*100, "INR")}
	}
	lines := func(ls []ai.Line) []aiLine {
		out := make([]aiLine, len(ls))
		for i, l := range ls {
			out[i] = line(l)
		}
		return out
	}
	av.Total = line(rep.Total)
	av.ByPurpose, av.ByModel, av.ByPerson, av.ByDay = lines(rep.ByPurpose), lines(rep.ByModel), lines(rep.ByPerson), lines(rep.ByDay)

	sv := settingsView{Tab: "ai", Tabs: s.tabs(c, "ai"), AI: &av}
	s.render(w, r, http.StatusOK, "settings", c.page(sv))
}

// count shows a token count in plain thousands ("1,234,567"), the way providers' bills show tokens.
func count(n int64) string {
	return groupDigits(strconv.FormatInt(n, 10), false)
}
