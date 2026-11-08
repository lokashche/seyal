// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/seal"
	"seyal/internal/workitem"
)

// Share out (Phase 4, part 4b-1, D91). The Work Item's Share panel lives in the sidebar (no new
// screen); the person's own browser hands the words to an app. Afterwards the page reports the
// hand-over here, and Seyal records it (item.shared). Nothing leaves the Seyal server.

// sharesPerMinute is how many hand-overs one person may record in a minute.
const sharesPerMinute = 30

// offerLife is how long a signed offer counts. The page drops a record still waiting after as long
// (app.js), so a phone keeps the words no longer than this.
const offerLife = 7 * 24 * time.Hour

// shareView is one message of the Share panel, with its signed offer (offerNote) for the record.
type shareView struct {
	workitem.ShareText
	Offer string
}

// offerNote is what the page carries for each message it offers: who it was offered to, in which
// business, when, and the message as offered (workitem.ShareOffer). Signed with Server.shareKey; it
// holds nothing secret: ids, a version and a fingerprint of words the page shows anyway.
type offerNote struct {
	Tenant string `json:"t"`
	Person string `json:"p"`
	At     int64  `json:"a"`
	workitem.ShareOffer
}

// shares is the Share panel's messages on item it, each with its signed offer.
func (v *view) shares(it workitem.Item) ([]shareView, error) {
	list, err := workitem.Shares(v.ctx, v.tx, it, ledger.PersonActor(v.c.session.PersonID), shareEnv(v.c, v.origin, it.ID))
	if err != nil {
		return nil, err
	}
	out := make([]shareView, len(list))
	now := time.Now().Unix()
	for i, m := range list {
		offer, err := seal.Sign(v.shareKey, offerNote{Tenant: v.c.tenant.ID, Person: v.c.session.PersonID, At: now,
			ShareOffer: m.Offer(it)})
		if err != nil {
			return nil, err
		}
		out[i] = shareView{ShareText: m, Offer: offer}
	}
	return out, nil
}

// openOffer reads the offer a record came with. It returns nil when there is none, or it can't be
// counted: not this server's since its last start, for another business or person, or too old. The
// record is then judged by the rules as they are now (workitem.RecordShare).
func (s *Server) openOffer(c *call, token string, now time.Time) *workitem.ShareOffer {
	var n offerNote
	if token == "" || seal.Open(s.shareKey, token, &n) != nil || n.Tenant != c.tenant.ID || n.Person != c.session.PersonID {
		return nil
	}
	if at := time.Unix(n.At, 0); now.Sub(at) > offerLife || at.After(now.Add(5*time.Minute)) {
		return nil
	}
	return &n.ShareOffer
}

// shareEnv is what a share text needs for one item: its full address (or only its path, when the
// browser's address can't be used), the business's name and its currency.
func shareEnv(c *call, origin, itemID string) workitem.ShareEnv {
	return workitem.ShareEnv{Link: origin + "/t/" + c.tenant.Slug + "/items/" + itemID,
		Business: c.tenant.Name, Currency: c.tenant.Currency}
}

// shared records a hand-over the page reports: POST /t/{slug}/items/{id}/shared with the message
// (share), the way it left (route), the exact words (text), the page's attempt id and the message's
// signed offer (offer), so the record is judged by what Seyal offered then (openOffer). The page sends it
// with fetch's keepalive, so it survives the page changing; when the phone is offline the page keeps it
// and sends it the next time Seyal opens. The answer is JSON: a refusal (403) or a bad record (400)
// is final, so the page drops it; anything else it tries again later.
func (s *Server) shared(w http.ResponseWriter, r *http.Request, c *call) {
	if !s.shareRate.take(c.tenant.ID + "/" + c.session.PersonID) {
		tooMany(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That share couldn't be read."})
		return
	}
	rec := workitem.ShareRecord{ID: r.PostFormValue("share"), Route: r.PostFormValue("route"),
		Text: r.PostFormValue("text"), Attempt: r.PostFormValue("attempt")}
	origin, _ := s.origin(r)
	id := r.PathValue("id")
	offer := s.openOffer(c, r.PostFormValue("offer"), time.Now())
	var recorded bool
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		var err error
		recorded, err = workitem.RecordShare(r.Context(), tx, id, ledger.PersonActor(c.session.PersonID), rec,
			shareEnv(c, origin, id), offer)
		return err
	})
	var bad *workitem.ShareInputError
	switch {
	case errors.Is(err, workitem.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "That item doesn't exist."})
	case errors.Is(err, workitem.ErrShareRefused):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "You can't share this message from this item now."})
	case errors.As(err, &bad):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That share couldn't be read."})
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "recorded": recorded})
	}
}
