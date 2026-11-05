//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/db"
)

type voiceAnswer struct{ Text, Raw, Token, Note string }

// speak sends a recording to be turned into words, as the composer does right after recording.
func (s *site) speak(c *http.Client, item string, audio []byte, contentType string) (int, voiceAnswer) {
	s.t.Helper()
	fields := map[string]string{}
	if item != "" {
		fields["item"] = item
	}
	code, body, _ := s.upload(c, "/t/"+s.slug+"/voice", fields, []upFile{{"voice", "voice-note.webm", contentType, audio}})
	var a voiceAnswer
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		s.t.Fatalf("voice answer = %d %q: %v", code, body, err)
	}
	return code, a
}

// sendSpoken sends a message with a recording and the token its words came with, as the composer
// does when the person presses Send. More fields (the process) can be added.
func (s *site) sendSpoken(c *http.Client, path, text, token string, audio []byte, more ...string) (int, string, http.Header) {
	s.t.Helper()
	fields := map[string]string{"text": text, "voice_token": token}
	for i := 0; i+1 < len(more); i += 2 {
		fields[more[i]] = more[i+1]
	}
	return s.upload(c, path, fields, []upFile{{"voice", "voice-note.webm", "audio/webm", audio}})
}

// voiceRequest is the request speak sends, for tests that send it themselves (several at the same
// time, or leaving before the answer).
func (s *site) voiceRequest(audio []byte) *http.Request {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="voice"; filename="voice-note.webm"`)
	h.Set("Content-Type", "audio/webm")
	part, _ := mw.CreatePart(h)
	part.Write(audio)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+"/t/"+s.slug+"/voice", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// aiCalls counts the calls for one purpose recorded in AI usage: all, and failed ones.
func (s *site) aiCalls(purpose string) (all, failed int) {
	s.t.Helper()
	err := db.WithTenant(context.Background(), s.app, s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT count(*), count(*) FILTER (WHERE NOT ok) FROM ai_calls WHERE tenant_id = current_tenant() AND purpose = $1`,
			purpose).Scan(&all, &failed)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return all, failed
}

func (s *site) transcribeCalls() (all, failed int) { return s.aiCalls("transcribe") }

// lastTranscribeCost is the cost recorded for the latest speech call.
func (s *site) lastTranscribeCost() (cost float64) {
	s.t.Helper()
	err := db.WithTenant(context.Background(), s.app, s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT cost_usd::float8 FROM ai_calls WHERE tenant_id = current_tenant() AND purpose = 'transcribe'
			ORDER BY created_at DESC LIMIT 1`).Scan(&cost)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return cost
}

// kept counts what is stored for voice notes: files, and transcripts.
func (s *site) kept() (files, transcripts int) {
	return s.count("SELECT count(*) FROM media"), s.count("SELECT count(*) FROM transcripts")
}

func TestSpeakInsteadOfType(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
	audio := ai.FakeRecording(2) // Seyal sends only recordings it can measure

	t.Run("off: voice notes work as before", func(t *testing.T) {
		if strings.Contains(home, "data-transcribe") {
			t.Error("the composer offers words from voice although no speech service is set")
		}
		if code, ans := s.speak(a, "", audio, "audio/webm"); code != http.StatusNotFound || ans.Token != "" {
			t.Errorf("voice with speech off = %d %+v", code, ans)
		}
	})

	speech := &ai.FakeSpeech{}
	s.ai.SetSpeech(ai.Speech{Name: "fake:test", Model: speech, PerHour: 0.36})
	understand := &ai.Fake{Answer: func(req ai.Request) (string, error) {
		kind := "note"
		switch {
		case strings.Contains(req.Input, "Hello Seyal"):
			kind = "greeting"
		case strings.Contains(req.Input, "Sundaram"):
			kind = "new_work"
		}
		return `{"kind":"` + kind + `","fields":{},"new_items":[],"command":"","reason":"","reply":""}`, nil
	}}
	s.ai.Set(ai.Understand, ai.Choice{Name: "fake:understand", Model: understand})

	var item string
	t.Run("Tell Seyal: the words come back to be corrected, nothing is kept until Send", func(t *testing.T) {
		_, page, _ := s.get(a, base+"/")
		if !strings.Contains(page, `data-transcribe="`+base+`/voice"`) {
			t.Fatal("the Tell Seyal composer doesn't offer words from voice")
		}
		files, transcripts := s.kept()
		code, ans := s.speak(a, "", audio, "audio/webm;codecs=opus")
		if code != http.StatusOK || ans.Token == "" || ans.Text != ai.FakeWords || ans.Raw != "" || ans.Note != "" {
			t.Fatalf("voice = %d %+v", code, ans)
		}
		if f, tr := s.kept(); f != files || tr != transcripts {
			t.Errorf("turning a voice note into words kept %d files and %d transcripts; want none before Send", f-files, tr-transcripts)
		}
		seen := speech.Seen()
		if len(seen) != 1 || string(seen[0].Data) != string(audio) || seen[0].ContentType != "audio/webm" {
			t.Fatalf("the speech service got %+v", seen)
		}
		if w := seen[0].Words; !strings.Contains(w, "Sundaram Textiles") || strings.Contains(w, "Arjun") || len(w) > 400 {
			t.Errorf("word list = %q: want names from the lists, no staff names, short", w)
		}

		corrected := "Sundaram Textiles wants monthly bookkeeping from April"
		code, body, h := s.sendSpoken(a, base+"/tell", corrected, ans.Token, audio, "playbook", pv)
		if code != http.StatusSeeOther {
			t.Fatalf("tell = %d: %s", code, problem(body))
		}
		if f, tr := s.kept(); f != files+1 || tr != transcripts+1 {
			t.Errorf("Send kept %d files and %d transcripts; want 1 and 1", f-files, tr-transcripts)
		}
		item = h.Get("Location")
		_, page, _ = s.get(a, item)
		if !strings.Contains(page, corrected) || !strings.Contains(page, `data-play="`+base+`/media/`) ||
			!strings.Contains(page, "corrected before sending") || strings.Contains(page, "🎙 Voice note") ||
			strings.Contains(page, "tidied by AI") {
			t.Errorf("the thread doesn't show the corrected words with a play button:\n%s", page)
		}
		if strings.Count(page, `data-transcribe="`+base+`/voice"`) != 1 || !strings.Contains(page, `data-item="`) ||
			!strings.Contains(page, "data-heard-toggle") {
			t.Error("the item's composer doesn't offer words from voice")
		}
		if seen := understand.Seen(); len(seen) == 0 || !strings.Contains(seen[len(seen)-1].Input, corrected) {
			t.Error("the corrected words weren't understood like typed text")
		}
		// The ledger: one event for the message, with the recording's fingerprint, the words heard and
		// the final text.
		body = s.lastEvent("message.received")
		if !strings.Contains(body, `"raw":"`+ai.FakeWords+`"`) || !strings.Contains(body, corrected) ||
			!strings.Contains(body, `"edited":true`) || !strings.Contains(body, `"sha256"`) {
			t.Errorf("message.received = %s", body)
		}
		s.tablesMatchLedger(t)
	})

	t.Run("in a work item: the item's own entries lead the word list", func(t *testing.T) {
		id := strings.TrimPrefix(item, base+"/items/")
		if code, ans := s.speak(a, id, audio, "audio/ogg"); code != http.StatusOK || ans.Text == "" {
			t.Fatalf("voice in an item = %d %+v", code, ans)
		}
		seen := speech.Seen()
		if w := seen[len(seen)-1].Words; w == "" || strings.Contains(w, "Arjun") {
			t.Errorf("word list = %q", w)
		}
	})

	t.Run("no words: nothing is kept, and the recording can be sent on its own", func(t *testing.T) {
		speech.Answer = func(ai.Audio) (ai.Transcript, error) { return ai.Transcript{Text: "  "}, nil }
		defer func() { speech.Answer = nil }()
		_, failedBefore := s.transcribeCalls()
		code, ans := s.speak(a, "", audio, "audio/webm")
		if code != http.StatusOK || ans.Token != "" || ans.Text != "" || !strings.Contains(ans.Note, "heard no words") ||
			!strings.Contains(ans.Note, "send it as a voice note") {
			t.Fatalf("voice without words = %d %+v", code, ans)
		}
		if _, failed := s.transcribeCalls(); failed != failedBefore+1 {
			t.Error("the failed call isn't in AI usage")
		}
		code, body, _ := s.upload(a, item+"/say", map[string]string{"text": ""}, []upFile{{"voice", "voice-note.webm", "audio/webm", audio}})
		if code != http.StatusSeeOther {
			t.Fatalf("say with the recording = %d: %s", code, problem(body))
		}
		if _, page, _ := s.get(a, item); !strings.Contains(page, "🎙 Voice note") {
			t.Error("the recording isn't in the thread as a voice note")
		}
	})

	t.Run("the service fails: no words, and the call isn't shown as free", func(t *testing.T) {
		speech.Answer = func(ai.Audio) (ai.Transcript, error) { return ai.Transcript{}, context.DeadlineExceeded }
		defer func() { speech.Answer = nil }()
		code, ans := s.speak(a, "", audio, "audio/webm")
		if code != http.StatusOK || ans.Token != "" || ans.Text != "" || !strings.Contains(ans.Note, "couldn't turn") {
			t.Fatalf("failed voice = %d %+v", code, ans)
		}
		// The service may charge for a call that failed after the recording reached it: the cost
		// uses the 2 seconds Seyal measured.
		if cost := s.lastTranscribeCost(); math.Abs(cost-2.0/3600*0.36) > 1e-9 {
			t.Errorf("failed call cost = %v, want the cost of 2 s", cost)
		}
	})

	t.Run("a recording Seyal can't measure isn't sent", func(t *testing.T) {
		before := len(speech.Seen())
		code, ans := s.speak(s.logIn(lakshmi), "", []byte("pretend opus recording"), "audio/webm")
		if code != http.StatusOK || ans.Token != "" || ans.Text != "" || !strings.Contains(ans.Note, "send it as a voice note") {
			t.Fatalf("unmeasured voice = %d %+v", code, ans)
		}
		if len(speech.Seen()) != before {
			t.Error("a recording of unknown length was sent to the speech service")
		}
	})

	t.Run("too long: not sent to the service", func(t *testing.T) {
		before := len(speech.Seen())
		code, ans := s.speak(a, "", make([]byte, ai.MaxAudio+1), "audio/webm")
		if code != http.StatusOK || ans.Token != "" || ans.Text != "" || !strings.Contains(ans.Note, "too long") {
			t.Fatalf("large voice = %d %+v", code, ans)
		}
		// A small file can still hold a long recording; the service charges by length.
		code, ans = s.speak(s.logIn(lakshmi), "", ai.FakeRecording(ai.MaxSeconds+60), "audio/webm")
		if code != http.StatusOK || ans.Token != "" || ans.Text != "" || !strings.Contains(ans.Note, "too long") {
			t.Fatalf("long voice = %d %+v", code, ans)
		}
		if len(speech.Seen()) != before {
			t.Error("a recording over the limit was sent to the speech service")
		}
	})

	t.Run("a spoken greeting is answered like a typed one: nothing starts", func(t *testing.T) {
		speech.Answer = func(ai.Audio) (ai.Transcript, error) { return ai.Transcript{Text: "Hello Seyal", Seconds: 2}, nil }
		defer func() { speech.Answer = nil }()
		_, ans := s.speak(a, "", audio, "audio/webm")
		if ans.Text != "Hello Seyal" {
			t.Fatalf("voice = %+v", ans)
		}
		code, body, h := s.sendSpoken(a, base+"/tell", "Hello Seyal", ans.Token, audio, "playbook", pv)
		if code != http.StatusOK || h.Get("Location") != "" || !strings.Contains(body, "Tell me about the work") {
			t.Errorf("spoken greeting = %d to %q: %s", code, h.Get("Location"), problem(body))
		}
	})

	t.Run("an audio file attached to a spoken message keeps its own player", func(t *testing.T) {
		_, before, _ := s.get(a, item)
		_, ans := s.speak(a, "", audio, "audio/webm")
		code, body, _ := s.upload(a, item+"/say", map[string]string{"text": ans.Text, "voice_token": ans.Token},
			[]upFile{{"voice", "voice-note.webm", "audio/webm", audio}, {"file", "call.mp3", "audio/mpeg", []byte("pretend phone call")}})
		if code != http.StatusSeeOther {
			t.Fatalf("say = %d: %s", code, problem(body))
		}
		_, after, _ := s.get(a, item)
		plays := strings.Count(after, "data-play=") - strings.Count(before, "data-play=")
		players := strings.Count(after, "<audio controls") - strings.Count(before, "<audio controls")
		if plays != 1 || players != 1 {
			t.Errorf("new play buttons %d, new audio players %d; want 1 and 1", plays, players)
		}
	})

	t.Run("every call is in AI usage", func(t *testing.T) {
		// Two with words, no words, failed, unmeasured, too large, too long, the greeting, the one with
		// a file.
		if all, failed := s.transcribeCalls(); all != 9 || failed != 5 {
			t.Errorf("speech calls = %d (failed %d), want 9 (5)", all, failed)
		}
		_, page, _ := s.get(s.logIn(lakshmi), base+"/settings/ai")
		if !strings.Contains(page, "Transcribe") || !strings.Contains(page, "fake:test") || !strings.Contains(page, "per hour of audio") {
			t.Error("Settings → AI doesn't show the speech service")
		}
	})

	t.Run("the words are the speaker's own: another person, a changed recording or no recording is refused", func(t *testing.T) {
		_, ans := s.speak(a, "", audio, "audio/webm")
		files, transcripts := s.kept()
		refused := func(why string, code int, body string, h http.Header) {
			t.Helper()
			if code != http.StatusUnprocessableEntity || h.Get("Seyal-Voice") != "refused" {
				t.Errorf("%s = %d (%q): %s", why, code, h.Get("Seyal-Voice"), problem(body))
			}
		}
		code, body, h := s.sendSpoken(s.logIn(priya), item+"/say", ans.Text, ans.Token, audio)
		refused("someone else's words", code, body, h)
		if !strings.Contains(problem(body), "can't check the words") {
			t.Errorf("someone else's words: %s", problem(body))
		}
		other := ai.FakeRecording(3)
		code, body, h = s.sendSpoken(a, item+"/say", ans.Text, ans.Token, other)
		refused("another recording", code, body, h)
		code, body, h = s.upload(a, item+"/say", map[string]string{"text": ans.Text, "voice_token": ans.Token}, nil)
		refused("words without their recording", code, body, h)
		code, body, h = s.sendSpoken(a, item+"/say", ans.Text, ans.Token[:len(ans.Token)-3]+"abc", audio)
		refused("a changed token", code, body, h)
		if f, tr := s.kept(); f != files || tr != transcripts {
			t.Errorf("refused messages kept %d files and %d transcripts", f-files, tr-transcripts)
		}
		if code, ans := s.speak(a, "", []byte("png"), "image/png"); code != http.StatusUnprocessableEntity || ans.Token != "" {
			t.Errorf("a photo as a voice note = %d %+v", code, ans)
		}
	})

	t.Run("voice guard: a spoken command never runs", func(t *testing.T) {
		speech.Answer = func(ai.Audio) (ai.Transcript, error) {
			return ai.Transcript{Text: "cancel because test", Seconds: 2}, nil
		}
		defer func() { speech.Answer = nil }()
		_, ans := s.speak(a, "", audio, "audio/webm")
		code, body, _ := s.sendSpoken(a, item+"/say", ans.Text, ans.Token, audio)
		if code != http.StatusSeeOther {
			t.Fatalf("say = %d: %s", code, problem(body))
		}
		id := strings.TrimPrefix(item, base+"/items/")
		if n := s.count("SELECT count(*) FROM work_items WHERE id::text = $1 AND state = 'cancelled'", id); n != 0 {
			t.Error("a spoken cancel cancelled the work")
		}
		if _, page, _ := s.get(a, item); !strings.Contains(page, "To do that, type it or tap the button.") ||
			!strings.Contains(page, "cancel because test") {
			t.Errorf("Seyal didn't explain, or the words weren't kept as a note:\n%s", page)
		}
		s.tablesMatchLedger(t)
	})

	t.Run("only logged-in people, only from Seyal's own pages", func(t *testing.T) {
		code, _, h := s.upload(s.browser(), base+"/voice", nil, []upFile{{"voice", "v.webm", "audio/webm", audio}})
		if code != http.StatusSeeOther || !strings.HasSuffix(h.Get("Location"), "/login") {
			t.Errorf("voice without login = %d to %q", code, h.Get("Location"))
		}
		req, _ := http.NewRequest(http.MethodPost, s.srv.URL+base+"/voice", strings.NewReader(""))
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		resp, err := a.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("voice from another site = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("the call is in AI usage even when the person leaves before the answer", func(t *testing.T) {
		speech.Answer = func(ai.Audio) (ai.Transcript, error) {
			time.Sleep(500 * time.Millisecond)
			return ai.Transcript{Text: "late words", Seconds: 3}, nil
		}
		defer func() { speech.Answer = nil }()
		all, _ := s.transcribeCalls()
		leaving := *a
		leaving.Timeout = 150 * time.Millisecond
		if resp, err := leaving.Do(s.voiceRequest(audio)); err == nil {
			resp.Body.Close()
			t.Fatal("the request wasn't given up on")
		}
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if now, _ := s.transcribeCalls(); now == all+1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the call made for a person who left isn't in AI usage")
			}
		}
	})

	t.Run("a speech call that panics gives its slot back", func(t *testing.T) {
		speech.Answer = func(ai.Audio) (ai.Transcript, error) { panic("test: the speech service's code broke") }
		l := s.logIn(lakshmi)
		if resp, err := l.Do(s.voiceRequest(audio)); err == nil { // net/http ends the request
			resp.Body.Close()
		}
		speech.Answer = nil
		if code, ans := s.speak(l, "", audio, "audio/webm"); code != http.StatusOK || ans.Token == "" {
			t.Errorf("the same login after a panic = %d %+v; want words (its slot given back)", code, ans)
		}
	})

	t.Run("a slow speech service doesn't hold up messages; one call per login, and one business gets only its share", func(t *testing.T) {
		release, entered := make(chan struct{}), make(chan struct{}, 4)
		speech.Answer = func(ai.Audio) (ai.Transcript, error) {
			entered <- struct{}{}
			<-release
			return ai.Transcript{Text: "slow words"}, nil
		}
		defer func() { speech.Answer = nil }()
		send := func(c *http.Client, answers chan<- voiceAnswer) {
			var ans voiceAnswer
			if resp, err := c.Do(s.voiceRequest(audio)); err == nil {
				json.NewDecoder(resp.Body).Decode(&ans)
				resp.Body.Close()
			}
			answers <- ans
		}
		waitEntered := func() {
			t.Helper()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("the speech service wasn't asked")
			}
		}
		busy := func(answers <-chan voiceAnswer, why string) {
			t.Helper()
			select {
			case ans := <-answers:
				if ans.Token != "" || ans.Text != "" || !strings.Contains(ans.Note, "can't turn more voice notes") {
					t.Errorf("%s = %+v", why, ans)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s: the voice note waited for the service", why)
			}
		}
		// One person sends two at once: one goes to the service, the other is answered at once.
		p, l := s.logIn(priya), s.logIn(lakshmi)
		slow := make(chan voiceAnswer, 3)
		go send(p, slow)
		go send(p, slow)
		waitEntered()
		busy(slow, "a second call of the same login")
		// Another person may use the business's second slot; a third person is over its share.
		go send(l, slow)
		waitEntered()
		over := make(chan voiceAnswer, 1)
		go send(a, over)
		busy(over, "over the business's share")
		// While the service is slow, a message sent with the composer's form (multipart) goes through.
		quick := *a
		quick.Timeout = 5 * time.Second
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("text", "Sent while the speech service is slow")
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, s.srv.URL+item+"/say", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := quick.Do(req)
		close(release)
		if err != nil {
			t.Fatalf("a message waited for the speech service: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("message = %d", resp.StatusCode)
		}
		for range 2 {
			if ans := <-slow; ans.Text != "slow words" {
				t.Errorf("slow answer = %+v", ans)
			}
		}
	})
}

// TestTidyTheWords: with the AI tidy-up on, the person gets the tidied words to check and can see
// the words as heard; the ledger keeps both and the final text. A tidy-up that adds a number is
// refused, and the words as heard are used. Every tidy-up call is in AI usage.
func TestTidyTheWords(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	audio := ai.FakeRecording(2)
	raw := "umm Sundaram Textiles, ah, Sundaram Textiles wants monthly books"
	s.ai.SetSpeech(ai.Speech{Name: "fake:test", Model: &ai.FakeSpeech{Answer: func(ai.Audio) (ai.Transcript, error) {
		return ai.Transcript{Text: raw, Seconds: 2}, nil
	}}})
	tidied := "Sundaram Textiles wants monthly books."
	tidy := &ai.Fake{Answer: func(r ai.Request) (string, error) {
		if !strings.Contains(r.Instructions, "Never add, remove or change facts") || r.Input != raw || r.Timeout <= 0 {
			t.Errorf("tidy request = %+v", r)
		}
		return `{"text":"` + tidied + `"}`, nil
	}}
	s.ai.Set(ai.Tidy, ai.Choice{Name: "fake:tidy", Model: tidy})

	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
	code, ans := s.speak(a, "", audio, "audio/webm")
	if code != http.StatusOK || ans.Text != tidied || ans.Raw != raw || ans.Token == "" {
		t.Fatalf("voice = %d %+v", code, ans)
	}
	code, body, h := s.sendSpoken(a, base+"/tell", ans.Text, ans.Token, audio, "playbook", pv)
	if code != http.StatusSeeOther {
		t.Fatalf("tell = %d: %s", code, problem(body))
	}
	_, page, _ := s.get(a, h.Get("Location"))
	if !strings.Contains(page, "spoken, tidied by AI</span>") {
		t.Errorf("the thread doesn't say the words were tidied (and not corrected):\n%s", page)
	}
	ev := s.lastEvent("message.received")
	if !strings.Contains(ev, `"raw":"`+raw+`"`) || !strings.Contains(ev, `"tidied":"`+tidied+`"`) ||
		!strings.Contains(ev, `"tidy_model":"fake:tidy"`) || !strings.Contains(ev, `"edited":false`) {
		t.Errorf("message.received = %s", ev)
	}
	s.tablesMatchLedger(t)

	// A tidy-up that adds a number isn't used: the person gets the words as heard.
	tidy.Answer = func(ai.Request) (string, error) { return `{"text":"Sundaram Textiles wants 12 monthly books."}`, nil }
	if _, ans = s.speak(a, "", audio, "audio/webm"); ans.Text != raw || ans.Raw != "" || ans.Token == "" {
		t.Errorf("a tidy-up with a new number = %+v, want the words as heard", ans)
	}
	// The tidy-up is down: the words as heard.
	tidy.Answer = func(ai.Request) (string, error) { return "", ai.ErrFakeDown }
	if _, ans = s.speak(a, "", audio, "audio/webm"); ans.Text != raw || ans.Token == "" {
		t.Errorf("tidy-up down = %+v, want the words as heard", ans)
	}
	if all, failed := s.aiCalls("tidy"); all != 3 || failed != 2 {
		t.Errorf("tidy calls = %d (failed %d), want 3 (2)", all, failed)
	}
	// AI usage is kept for good, also when the note is discarded: the reason a tidy-up was refused
	// never quotes the note or the answer.
	if n := s.count(`SELECT count(*) FROM ai_calls WHERE purpose = 'tidy' AND error = 'tidy-up refused: it has a number that wasn''t said'`); n != 1 {
		t.Errorf("refused tidy-ups recorded with the fixed reason = %d, want 1", n)
	}
	if n := s.count(`SELECT count(*) FROM ai_calls WHERE error LIKE '%12%' OR error LIKE '%Sundaram%'`); n != 0 {
		t.Errorf("%d AI usage rows quote the voice note", n)
	}
}

// TestSpeechMinutesADay: minutes of audio are counted, so one login can't send note after note to
// the paid service. Over the login's minutes for the day, the service isn't asked.
func TestSpeechMinutesADay(t *testing.T) {
	s := newSite(t)
	speech := &ai.FakeSpeech{}
	s.ai.SetSpeech(ai.Speech{Name: "fake:test", Model: speech, LoginDayMinutes: 1})
	l := s.logIn(lakshmi)
	if _, ans := s.speak(l, "", ai.FakeRecording(40), "audio/webm"); ans.Token == "" {
		t.Fatalf("the first note = %+v", ans)
	}
	_, ans := s.speak(l, "", ai.FakeRecording(40), "audio/webm")
	if ans.Token != "" || ans.Text != "" || !strings.Contains(ans.Note, "today") || !strings.Contains(ans.Note, "send it as a voice note") {
		t.Errorf("over the login's minutes a day = %+v", ans)
	}
	if got := len(speech.Seen()); got != 1 {
		t.Errorf("the service was asked %d times, want 1", got)
	}
	// The refused note gave its slot back, and another login has its own minutes.
	if _, ans := s.speak(s.logIn(arjun), "", ai.FakeRecording(40), "audio/webm"); ans.Token == "" {
		t.Errorf("another login's note = %+v", ans)
	}
	if all, _ := s.transcribeCalls(); all != 2 {
		t.Errorf("transcribe calls recorded = %d, want 2 (a refused note costs nothing)", all)
	}
}

// TestSpeechRateLimit: only a few voice notes per login per minute go to the speech service.
func TestSpeechRateLimit(t *testing.T) {
	s := newSite(t)
	speech := &ai.FakeSpeech{}
	s.ai.SetSpeech(ai.Speech{Name: "fake:test", Model: speech})
	l := s.logIn(lakshmi)
	var last voiceAnswer
	for range 11 {
		_, last = s.speak(l, "", ai.FakeRecording(1), "audio/webm")
	}
	if got := len(speech.Seen()); got != 10 {
		t.Errorf("the service was asked %d times, want 10", got)
	}
	if last.Token != "" || last.Text != "" || !strings.Contains(last.Note, "send it as a voice note") {
		t.Errorf("over the limit = %+v", last)
	}
}

// lastEvent returns the body of the latest ledger event of a kind.
func (s *site) lastEvent(kind string) (body string) {
	s.t.Helper()
	err := db.WithTenant(context.Background(), s.app, s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT body FROM ledger WHERE tenant_id = current_tenant() AND kind = $1 ORDER BY seq DESC LIMIT 1`, kind).Scan(&body)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return body
}

// TestSpokenMessageToConfirm: when Seyal asks which process a spoken message is for, the recording
// the words were spoken in shows as a play button, not as an attached file (as in the thread).
func TestSpokenMessageToConfirm(t *testing.T) {
	s := newSite(t)
	s.twoProcesses()
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	audio := ai.FakeRecording(2)
	words := "The printer in my office is broken again"
	s.ai.SetSpeech(ai.Speech{Name: "fake:test", Model: &ai.FakeSpeech{Answer: func(ai.Audio) (ai.Transcript, error) {
		return ai.Transcript{Text: words, Seconds: 2}, nil
	}}})

	code, ans := s.speak(a, "", audio, "audio/webm")
	if code != http.StatusOK || ans.Token == "" {
		t.Fatalf("voice = %d %+v", code, ans)
	}
	code, page, _ := s.sendSpoken(a, base+"/tell", words, ans.Token, audio, "playbook", "auto")
	if code != http.StatusOK || !strings.Contains(page, "Start it?") {
		t.Fatalf("tell = %d, want a suggestion to confirm:\n%s", code, problem(page))
	}
	if strings.Contains(page, "attached") || !strings.Contains(page, `data-play="`+base+`/media/`) {
		t.Errorf("the spoken message's card should show a play button and no attached file:\n%s", problem(page))
	}

	// A photo sent with the spoken words is an attached file.
	code, ans = s.speak(a, "", audio, "audio/webm")
	if code != http.StatusOK || ans.Token == "" {
		t.Fatalf("voice = %d %+v", code, ans)
	}
	code, page, _ = s.upload(a, base+"/tell", map[string]string{"text": words, "voice_token": ans.Token, "playbook": "auto"},
		[]upFile{{"voice", "voice-note.webm", "audio/webm", audio}, {"file", "printer.jpg", "image/jpeg", []byte("\xff\xd8\xff pretend jpeg")}})
	if code != http.StatusOK || !strings.Contains(page, "1 file attached") || !strings.Contains(page, `data-play="`) {
		t.Errorf("with a photo = %d, want a play button and 1 file attached:\n%s", code, problem(page))
	}
}
