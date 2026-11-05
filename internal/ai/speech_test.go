// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSpeechService pretends to be an OpenAI-style speech service on this computer. It records what
// it was sent and answers with reply (or status, or after a delay).
type fakeSpeechService struct {
	auth, contentType string
	fields            map[string]string
	file              []byte
	fileName, fileCT  string
	status            int
	delay             time.Duration
	reply             string
	onlyJSON          bool // refuse response_format=verbose_json, as some newer speech models do
	calls             int
}

func (f *fakeSpeechService) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.auth, f.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("not a multipart form: %v", err)
			return
		}
		f.calls++
		f.fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			f.fields[k] = v[0]
		}
		if f.onlyJSON && f.fields["response_format"] != "json" {
			http.Error(w, `{"error":{"message":"response_format 'verbose_json' is not compatible with this model"}}`, http.StatusBadRequest)
			return
		}
		if fh := r.MultipartForm.File["file"]; len(fh) == 1 {
			file, _ := fh[0].Open()
			f.file, _ = io.ReadAll(file)
			file.Close()
			f.fileName, f.fileCT = fh[0].Filename, fh[0].Header.Get("Content-Type")
		}
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		if f.status != 0 {
			http.Error(w, `{"error":{"message":"file too large"}}`, f.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, f.reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// rec is a short recording Seyal can measure: 3 seconds of sound.
var rec = FakeRecording(3)

const spokenReply = `{"text":" Sundaram Textiles-க்கு monthly bookkeeping start பண்ணுங்க ","language":"tamil","duration":12.5}`

func speechEnv(base string) func(string) string {
	return func(k string) string {
		return map[string]string{
			"SEYAL_AI_TRANSCRIBE": "groq:a-model", "SEYAL_AI_TRANSCRIBE_PRICE": "0.36",
			"SEYAL_GROQ_API_KEY": "test-key-not-real", "SEYAL_GROQ_BASE_URL": base,
		}[k]
	}
}

func TestWhisperSendsAndReads(t *testing.T) {
	svc := &fakeSpeechService{reply: spokenReply}
	s, err := FromEnv(speechEnv(svc.start(t)))
	if err != nil {
		t.Fatal(err)
	}
	audio := FakeRecording(12)
	tr, call, err := s.Transcribe(context.Background(), Audio{Data: audio, ContentType: "audio/webm", Words: "Sundaram Textiles, Ganga Traders"},
		Link{PersonID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "Sundaram Textiles-க்கு monthly bookkeeping start பண்ணுங்க" || tr.Language != "tamil" || tr.Seconds != 12.5 {
		t.Errorf("transcript = %+v", tr)
	}
	if svc.auth != "Bearer test-key-not-real" || !strings.HasPrefix(svc.contentType, "multipart/form-data; boundary=") {
		t.Errorf("headers: auth %q, type %q", svc.auth, svc.contentType)
	}
	if string(svc.file) != string(audio) || svc.fileName != "voice.webm" || svc.fileCT != "audio/webm" {
		t.Errorf("file sent: %q as %q (%q)", svc.file, svc.fileName, svc.fileCT)
	}
	want := map[string]string{"model": "a-model", "response_format": "verbose_json", "temperature": "0",
		"prompt": "Sundaram Textiles, Ganga Traders"}
	for k, v := range want {
		if svc.fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, svc.fields[k], v)
		}
	}
	if _, set := svc.fields["language"]; set {
		t.Error("a language was sent although none is set: mixed speech needs the service to detect it")
	}
	if call.Purpose != Transcribe || call.Model != "groq:a-model" || call.Err != nil || call.Link.PersonID != "p1" ||
		math.Abs(call.CostUSD-12.5/3600*0.36) > 1e-12 {
		t.Errorf("call = %+v", call)
	}
}

func TestWhisperPlainJSONModels(t *testing.T) {
	svc := &fakeSpeechService{reply: `{"text":"vanakkam"}`, onlyJSON: true}
	s, err := FromEnv(speechEnv(svc.start(t)))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		tr, call, err := s.Transcribe(context.Background(), Audio{Data: rec, ContentType: "audio/webm"}, Link{})
		// The plain answer has no length: the cost uses the length Seyal measured (3 s).
		if err != nil || tr.Text != "vanakkam" || call.Err != nil || math.Abs(call.CostUSD-3.0/3600*0.36) > 1e-12 {
			t.Fatalf("call %d: %+v %+v %v", i+1, tr, call, err)
		}
	}
	if svc.calls != 3 || svc.fields["response_format"] != "json" {
		t.Errorf("service called %d times (want 3: refused once, then plain json twice), last format %q",
			svc.calls, svc.fields["response_format"])
	}
}

func TestWhisperLanguageHint(t *testing.T) {
	svc := &fakeSpeechService{reply: `{"text":"vanakkam"}`}
	base := svc.start(t)
	s, err := FromEnv(func(k string) string {
		if k == "SEYAL_AI_TRANSCRIBE_LANGUAGE" {
			return "TA"
		}
		return speechEnv(base)(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Transcribe(context.Background(), Audio{Data: rec, ContentType: "audio/ogg"}, Link{}); err != nil {
		t.Fatal(err)
	}
	if svc.fields["language"] != "ta" || svc.fileName != "voice.ogg" {
		t.Errorf("language %q, file %q", svc.fields["language"], svc.fileName)
	}
	if _, has := svc.fields["prompt"]; has {
		t.Error("an empty word list was sent")
	}
}

func TestWhisperFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("an error answer is reported without the key", func(t *testing.T) {
		svc := &fakeSpeechService{status: http.StatusRequestEntityTooLarge}
		s, _ := FromEnv(speechEnv(svc.start(t)))
		_, call, err := s.Transcribe(ctx, Audio{Data: rec, ContentType: "audio/webm"}, Link{})
		if err == nil || !strings.Contains(err.Error(), "413") || call.Err == nil {
			t.Fatalf("err = %v, call.Err = %v", err, call.Err)
		}
		if strings.Contains(err.Error(), "test-key-not-real") {
			t.Error("the key appears in the error")
		}
	})
	t.Run("a slow service is given up on", func(t *testing.T) {
		svc := &fakeSpeechService{delay: 2 * time.Second, reply: `{"text":"late"}`}
		base := svc.start(t)
		w := &whisper{provider: "groq", model: "m", url: base + "/audio/transcriptions", timeout: 50 * time.Millisecond}
		s := &Setup{}
		s.SetSpeech(Speech{Name: "groq:m", Model: w})
		start := time.Now()
		_, call, err := s.Transcribe(ctx, Audio{Data: rec, ContentType: "audio/webm"}, Link{})
		if !errors.Is(err, context.DeadlineExceeded) || call.Err == nil || time.Since(start) > time.Second {
			t.Errorf("err = %v after %v", err, time.Since(start))
		}
	})
	t.Run("a call that failed after the recording was sent is not shown as free", func(t *testing.T) {
		svc := &fakeSpeechService{delay: 2 * time.Second, reply: `{"text":"late"}`}
		base := svc.start(t)
		w := &whisper{provider: "groq", model: "m", url: base + "/audio/transcriptions", timeout: 50 * time.Millisecond}
		s := &Setup{}
		s.SetSpeech(Speech{Name: "groq:m", Model: w, PerHour: 0.36})
		_, call, err := s.Transcribe(ctx, Audio{Data: rec, ContentType: "audio/webm"}, Link{})
		if err == nil || math.Abs(call.CostUSD-3.0/3600*0.36) > 1e-12 {
			t.Errorf("timed out: err %v, cost %v; want the cost of the measured 3 s", err, call.CostUSD)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel() // the person left before the answer
		if _, call, err := s.Transcribe(cancelled, Audio{Data: rec, ContentType: "audio/webm"}, Link{}); err == nil || call.CostUSD == 0 {
			t.Errorf("cancelled: err %v, cost %v", err, call.CostUSD)
		}
	})
	t.Run("an answer that isn't JSON", func(t *testing.T) {
		svc := &fakeSpeechService{reply: "<html>gateway</html>"}
		s, _ := FromEnv(speechEnv(svc.start(t)))
		if _, _, err := s.Transcribe(ctx, Audio{Data: rec, ContentType: "audio/webm"}, Link{}); err == nil {
			t.Error("a broken answer was accepted")
		}
	})
	t.Run("silence gives no words", func(t *testing.T) {
		svc := &fakeSpeechService{reply: `{"text":"   "}`}
		s, _ := FromEnv(speechEnv(svc.start(t)))
		if _, _, err := s.Transcribe(ctx, Audio{Data: rec, ContentType: "audio/webm"}, Link{}); !errors.Is(err, ErrNoWords) {
			t.Errorf("err = %v, want ErrNoWords", err)
		}
	})
	t.Run("too large, too long, unmeasured or empty recordings are not sent", func(t *testing.T) {
		svc := &fakeSpeechService{reply: `{"text":"x"}`}
		s, _ := FromEnv(speechEnv(svc.start(t)))
		if _, call, err := s.Transcribe(ctx, Audio{Data: make([]byte, MaxAudio+1), ContentType: "audio/webm"}, Link{}); !errors.Is(err, ErrTooLarge) || call.Err == nil {
			t.Errorf("too large: %v", err)
		}
		// Small (under 1 MB), but longer than MaxSeconds: services charge by length.
		long := FakeRecording(MaxSeconds + 1)
		if _, call, err := s.Transcribe(ctx, Audio{Data: long, ContentType: "audio/webm"}, Link{}); !errors.Is(err, ErrTooLarge) || call.Err == nil || call.CostUSD != 0 {
			t.Errorf("too long (%d bytes): %v, cost %v", len(long), err, call.CostUSD)
		}
		if _, _, err := s.Transcribe(ctx, Audio{Data: []byte("pretend opus audio"), ContentType: "audio/webm"}, Link{}); !errors.Is(err, ErrUnmeasured) {
			t.Errorf("unmeasured: %v", err)
		}
		if _, _, err := s.Transcribe(ctx, Audio{ContentType: "audio/webm"}, Link{}); err == nil {
			t.Error("an empty recording was sent")
		}
		if svc.fields != nil {
			t.Error("the service was called for a recording it shouldn't get")
		}
	})
	t.Run("an unknown recording type is not sent", func(t *testing.T) {
		svc := &fakeSpeechService{reply: `{"text":"x"}`}
		s, _ := FromEnv(speechEnv(svc.start(t)))
		if _, _, err := s.Transcribe(ctx, Audio{Data: rec, ContentType: "image/png"}, Link{}); err == nil || svc.fields != nil {
			t.Errorf("image sent as audio: %v", err)
		}
	})
}

func TestSpeechSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	s, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, on := s.Speech(); on {
		t.Error("speech is on with nothing set")
	}
	if _, _, err := s.Transcribe(context.Background(), Audio{Data: []byte("x")}, Link{}); !errors.Is(err, ErrOff) {
		t.Errorf("transcribe with speech off = %v, want ErrOff", err)
	}
	if _, on := (*Setup)(nil).Speech(); on {
		t.Error("a nil setup has speech on")
	}
	s, err = FromEnv(env(map[string]string{"SEYAL_AI_TRANSCRIBE": "fake:demo", "SEYAL_AI_TRANSCRIBE_PRICE": "0.04"}))
	if err != nil {
		t.Fatal(err)
	}
	sp, on := s.Speech()
	if !on || sp.Name != "fake:demo" || sp.PerHour != 0.04 || sp.Language != "" ||
		sp.DayMinutes != DefaultDayMinutes || sp.LoginDayMinutes != DefaultLoginDayMinutes {
		t.Errorf("speech = %+v, on %v", sp, on)
	}
	s, err = FromEnv(env(map[string]string{"SEYAL_AI_TRANSCRIBE": "fake:demo",
		"SEYAL_AI_TRANSCRIBE_DAY_MINUTES": "600", "SEYAL_AI_TRANSCRIBE_LOGIN_DAY_MINUTES": " 30 "}))
	if sp, _ := s.Speech(); err != nil || sp.DayMinutes != 600 || sp.LoginDayMinutes != 30 {
		t.Errorf("minutes a day = %+v, %v", sp, err)
	}
	tr, _, err := s.Transcribe(context.Background(), Audio{Data: rec, ContentType: "audio/webm"}, Link{})
	if err != nil || tr.Text != FakeWords {
		t.Errorf("fake transcript = %+v, %v", tr, err)
	}
	for _, bad := range []map[string]string{
		{"SEYAL_AI_TRANSCRIBE": "no-colon"},
		{"SEYAL_AI_TRANSCRIBE": "nosuch:x"},
		{"SEYAL_AI_TRANSCRIBE": "groq:x"}, // no key
		{"SEYAL_AI_TRANSCRIBE": "fake:x", "SEYAL_AI_TRANSCRIBE_PRICE": "cheap"},
		{"SEYAL_AI_TRANSCRIBE": "fake:x", "SEYAL_AI_TRANSCRIBE_LANGUAGE": "Tamil"},
		{"SEYAL_AI_TRANSCRIBE": "fake:x", "SEYAL_AI_TRANSCRIBE_DAY_MINUTES": "0"},
		{"SEYAL_AI_TRANSCRIBE": "fake:x", "SEYAL_AI_TRANSCRIBE_LOGIN_DAY_MINUTES": "an hour"},
	} {
		if _, err := FromEnv(env(bad)); err == nil {
			t.Errorf("settings %v were accepted", bad)
		}
	}
}
