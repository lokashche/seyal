// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/ask"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/seal"
	"seyal/internal/workitem"
)

// Speak instead of type (D66). The composer sends a voice note here as soon as it is recorded. Seyal
// asks the speech service for the words (and the AI tidy-up, if it is on, for a cleaner version),
// and answers with them, so they appear in the message box for the person to correct and send:
//
//	{"text": "the words to check", "raw": "the words as heard", "token": "<signed>"}
//	{"note": "why there are no words"}   (the browser still has the recording; it can be sent as it is)
//
// Nothing is kept here: the browser holds the recording. When the person presses Send, the recording
// goes with the message, together with the token. The token is signed by the server (seal), so the
// browser can't change what was heard; heardFrom checks it, and the message's transaction keeps the
// recording, the words heard and the final text together (channel.Receive). Discarding the voice
// note, or leaving the page, keeps nothing on the server.

type voiceAnswer struct {
	Text  string `json:"text,omitempty"`  // the words to check: tidied, or else as heard
	Raw   string `json:"raw,omitempty"`   // the words as heard, when the tidy-up changed them
	Token string `json:"token,omitempty"` // what was heard, signed; sent back with the message
	Note  string `json:"note,omitempty"`
}

const (
	noteFailed = "Seyal couldn't turn this voice note into words just now. You can still send it as a voice note, or type the words."
	noteNoWord = "Seyal heard no words in this voice note. You can still send it as a voice note, or record it again."
	noteLong   = "This voice note is too long to turn into words. You can still send it as a voice note."
	noteBusy   = "Seyal can't turn more voice notes into words just now. You can still send it as a voice note, or type the words."
	noteToday  = "Seyal has turned as many minutes of voice notes into words today as it may. You can still send it as a voice note, or type the words."
)

// Limits on the speech service, which is paid per minute of audio and may be slow (D66):
//   - at most speechAtOnce calls wait on the service at the same time on the whole server, and at
//     most speechPerBusiness of them for one business, so one business can't use up the others' share;
//   - one login has at most one call waiting at a time, so one person can't hold their business's
//     share, and one login used in several businesses can't hold the whole server's;
//   - at most speechPerMinute voice notes per login per minute are sent to it;
//   - a recording is sent only if Seyal can measure its length, up to ai.MaxSeconds (the service
//     charges by length, so no single call can be made long on purpose);
//   - minutes of audio are counted too (audioUse): one login sends at most speechShortMinutes in any
//     speechShortWindow (about as fast as one can speak), and in any 24 hours at most the minutes
//     set for a login and for a business (ai.Speech: SEYAL_AI_TRANSCRIBE_LOGIN_DAY_MINUTES and
//     SEYAL_AI_TRANSCRIBE_DAY_MINUTES). So one login, or a stolen login cookie, can't run up a big
//     bill by sending note after note.
//
// A voice note over a limit isn't turned into words; the person can still send it as a voice note.
// The tidy-up runs inside the same slot, right after the speech service.
const (
	speechAtOnce       = 4
	speechPerBusiness  = 2
	speechPerMinute    = 10
	speechShortMinutes = 10
	speechShortWindow  = 10 * time.Minute
)

// heardFor is how long the words heard stay good for sending (the token's age). The person checks
// and corrects the words right after recording; a token older than this is refused, so a token that
// leaked somewhere can't be used much later. Seyal also forgets its signing key when it restarts.
const heardFor = time.Hour

// speechSlots counts the calls waiting on the speech service: on the whole server, per business, and
// per login (at most one each). Nobody waits for a slot: when there is none, the voice note gets no
// words.
type speechSlots struct {
	mu          sync.Mutex
	max, most   int // on the whole server, per business
	busy        int
	perBusiness map[string]int
	perLogin    map[string]bool
}

func newSpeechSlots(max, perBusiness int) *speechSlots {
	return &speechSlots{max: max, most: perBusiness, perBusiness: map[string]int{}, perLogin: map[string]bool{}}
}

// take reports whether login may call the service now for business, and if so counts the call.
// Every take that returns true must be followed by one give with the same business and login.
func (s *speechSlots) take(business, login string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy >= s.max || s.perBusiness[business] >= s.most || s.perLogin[login] {
		return false
	}
	s.busy++
	s.perBusiness[business]++
	s.perLogin[login] = true
	return true
}

// give hands back the slot of a call that has ended.
func (s *speechSlots) give(business, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy--
	if s.perBusiness[business]--; s.perBusiness[business] <= 0 {
		delete(s.perBusiness, business)
	}
	delete(s.perLogin, login)
}

// audioUse counts the minutes of audio sent to the speech service, per login and per business, over
// the last 24 hours. It is kept in memory: a restart starts the count again.
type audioUse struct {
	mu    sync.Mutex
	sent  map[string][]audioSent // by "login " + login, or "business " + business
	swept time.Time
}

type audioSent struct {
	at      time.Time
	seconds float64
}

// audioLimits are the most seconds of audio: for one login in speechShortWindow, and in 24 hours for
// one login and for one business.
type audioLimits struct{ loginShort, loginDay, businessDay float64 }

func newAudioUse() *audioUse { return &audioUse{sent: map[string][]audioSent{}} }

// take reports whether login may send seconds more audio for business now, and if so counts them.
// It says no when a limit is already used up, or would be passed. today reports that a 24-hour limit
// (not the short one) said no.
func (u *audioUse) take(business, login string, seconds float64, lim audioLimits, now time.Time) (ok, today bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if now.Sub(u.swept) > time.Hour { // forget logins and businesses that sent nothing for a day
		for key := range u.sent {
			u.recent(key, now)
		}
		u.swept = now
	}
	within := func(max, used float64) bool { return used < max && used+seconds <= max }
	loginKey, businessKey := "login "+login, "business "+business
	loginDay, loginShort := u.used(loginKey, now)
	businessDay, _ := u.used(businessKey, now)
	switch {
	case !within(lim.loginDay, loginDay) || !within(lim.businessDay, businessDay):
		return false, true
	case !within(lim.loginShort, loginShort):
		return false, false
	}
	for _, key := range []string{loginKey, businessKey} {
		u.sent[key] = append(u.sent[key], audioSent{at: now, seconds: seconds})
	}
	return true, false
}

// used is how many seconds key sent in the last 24 hours, and in the last speechShortWindow. The
// caller holds the lock.
func (u *audioUse) used(key string, now time.Time) (day, short float64) {
	for _, a := range u.recent(key, now) {
		day += a.seconds
		if now.Sub(a.at) < speechShortWindow {
			short += a.seconds
		}
	}
	return day, short
}

// recent drops what key sent over 24 hours ago, and returns the rest. The caller holds the lock.
func (u *audioUse) recent(key string, now time.Time) []audioSent {
	kept := u.sent[key][:0]
	for _, a := range u.sent[key] {
		if now.Sub(a.at) < 24*time.Hour {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		delete(u.sent, key)
		return nil
	}
	u.sent[key] = kept
	return kept
}

// speechLimits are the limits of audio for the speech service sp.
func speechLimits(sp ai.Speech) audioLimits {
	return audioLimits{loginShort: speechShortMinutes * 60,
		loginDay:    float64(cmp.Or(sp.LoginDayMinutes, ai.DefaultLoginDayMinutes)) * 60,
		businessDay: float64(cmp.Or(sp.DayMinutes, ai.DefaultDayMinutes)) * 60}
}

// heardNote is what a voice token says: what was heard, for whom, and when.
type heardNote struct {
	Tenant string        `json:"tenant"`
	Person string        `json:"person"`
	At     int64         `json:"at"` // Unix seconds, when the words were heard
	Heard  channel.Heard `json:"heard"`
}

// errHeard means a message came with words from a voice note that Seyal can't vouch for: the token
// is broken, for another business or person, or too old.
var errHeard = errors.New("Seyal can't check the words of this voice note any more (they are over an hour old, " +
	"or Seyal was restarted). Record it again, or send it as a voice note")

// heardFrom checks a voice token from the composer and returns what was heard. It must have been
// made by this server (since it last started), for this business and this person, within heardFor.
func (s *Server) heardFrom(token string, c *call, now time.Time) (channel.Heard, error) {
	var n heardNote
	if err := seal.Open(s.voiceKey, token, &n); err != nil {
		return channel.Heard{}, errHeard
	}
	age := now.Sub(time.Unix(n.At, 0))
	if n.Tenant != c.tenant.ID || n.Person != c.session.PersonID || age > heardFor || age < -time.Minute {
		return channel.Heard{}, errHeard
	}
	return n.Heard, nil
}

func (s *Server) voice(w http.ResponseWriter, r *http.Request, c *call) {
	answer := func(status int, a voiceAnswer) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(a)
	}
	sp, on := s.ai.Speech()
	if !on {
		answer(http.StatusNotFound, voiceAnswer{Note: "Turning voice notes into words is switched off."})
		return
	}
	data, ct, status, note, err := readVoice(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if note != "" {
		answer(status, voiceAnswer{Note: note})
		return
	}
	itemID := r.FormValue("item")

	// 1. Gather the word list (nothing is written).
	var words string
	err = db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		if itemID != "" {
			if _, err := workitem.Get(r.Context(), tx, itemID); errors.Is(err, workitem.ErrNotFound) {
				itemID = "" // not an item of this business: no word list from it, no link in AI usage
			} else if err != nil {
				return err
			}
		}
		var err error
		words, err = ask.VoiceWords(r.Context(), tx, itemID)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// 2. Ask for the words, then the tidy-up, outside any database transaction: no transaction waits
	// on a service. Over a limit, the service isn't asked at all. The per-person limits count the
	// login, which may be used in several businesses (D64).
	login := c.session.AccountID
	if login == "" {
		login = c.tenant.ID + "/" + c.session.PersonID
	}
	if !s.voiceRate.take(login) || !s.speaking.take(c.tenant.ID, login) {
		answer(http.StatusOK, voiceAnswer{Note: noteBusy})
		return
	}
	if ok, today := s.audioUsed.take(c.tenant.ID, login, ai.SentSeconds(data), speechLimits(sp), time.Now()); !ok {
		s.speaking.give(c.tenant.ID, login)
		note := noteBusy
		if today {
			note = noteToday
		}
		answer(http.StatusOK, voiceAnswer{Note: note})
		return
	}
	link := ai.Link{PersonID: c.session.PersonID, ItemID: itemID}
	var (
		tr       ai.Transcript
		called   ai.Called
		callErr  error
		tidied   string
		tidyCall *ai.Called
	)
	func() {
		defer s.speaking.give(c.tenant.ID, login) // even if a call panics, the slot comes back
		tr, called, callErr = s.ai.Transcribe(r.Context(), ai.Audio{Data: data, ContentType: ct, Words: words}, link)
		if callErr == nil {
			tidied, tidyCall = ask.Tidy(r.Context(), s.ai, tr.Text, link)
		}
	}()

	// 3. Record the calls in AI usage (failed ones too). This goes on even when the person has left
	// meanwhile (the services may have charged for the calls).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	err = db.WithTenant(ctx, s.app, c.tenant.ID, func(tx pgx.Tx) error {
		if err := called.Record(ctx, tx); err != nil || tidyCall == nil {
			return err
		}
		return tidyCall.Record(ctx, tx)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	switch {
	case errors.Is(callErr, ai.ErrNoWords):
		answer(http.StatusOK, voiceAnswer{Note: noteNoWord})
		return
	case errors.Is(callErr, ai.ErrTooLarge):
		answer(http.StatusOK, voiceAnswer{Note: noteLong})
		return
	case callErr != nil:
		// The error names the service and its answer, never the key.
		slog.Warn("voice note not turned into words", "tenant", c.tenant.Slug, "error", callErr)
		answer(http.StatusOK, voiceAnswer{Note: noteFailed})
		return
	}

	// 4. Sign what was heard, for the message the person sends.
	h := channel.Heard{SHA256: media.Fingerprint(data), Model: called.Model, Language: tr.Language, Raw: tr.Text}
	if tidied != "" && tidyCall != nil {
		h.Tidied, h.TidyModel = tidied, tidyCall.Model
	}
	token, err := seal.Sign(s.voiceKey, heardNote{Tenant: c.tenant.ID, Person: c.session.PersonID, At: time.Now().Unix(), Heard: h})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	a := voiceAnswer{Text: h.Given(), Token: token}
	if a.Text != h.Raw {
		a.Raw = h.Raw
	}
	answer(http.StatusOK, a)
}

// readVoice reads the one recording in the request, with its plain content type. Recordings are
// held in memory, so only a few requests are read at once (the uploads slots, shared with incoming).
// The slot is given back as soon as the recording is read, before the speech service is asked, so a
// slow service never keeps other people's messages waiting. A non-empty note (with its status) means
// the request is refused.
func readVoice(w http.ResponseWriter, r *http.Request) (data []byte, ct string, status int, note string, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxSize+1<<20)
	select {
	case uploads <- struct{}{}:
		defer func() { <-uploads }()
	case <-r.Context().Done():
		return nil, "", 0, "", r.Context().Err()
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return nil, "", http.StatusRequestEntityTooLarge, "The voice note is too large.", nil
	}
	files := r.MultipartForm.File["voice"]
	if len(files) != 1 || files[0].Size == 0 || files[0].Size > media.MaxSize {
		return nil, "", http.StatusUnprocessableEntity, "Send one voice note.", nil
	}
	ct, ok := media.Clean(files[0].Header.Get("Content-Type"))
	if !ok || !strings.HasPrefix(ct, "audio/") {
		return nil, "", http.StatusUnprocessableEntity, "Only voice recordings can be turned into words.", nil
	}
	f, err := files[0].Open()
	if err != nil {
		return nil, "", 0, "", err
	}
	defer f.Close()
	if data, err = io.ReadAll(f); err != nil {
		return nil, "", 0, "", err
	}
	return data, ct, 0, "", nil
}
