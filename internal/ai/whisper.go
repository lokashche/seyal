// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync/atomic"
	"time"
)

// Speech to text at an OpenAI-style service ("audio/transcriptions"), e.g. speech models at Groq or
// OpenAI, or a speech server on our own machine (compat). The recording is sent as a multipart form,
// the way a browser uploads a file. The key goes only in the Authorization header.

// speechTimeout limits one request. A five-minute recording usually takes a few seconds.
const speechTimeout = time.Minute

func newWhisper(p preset, model string, get func(string) string) (Transcriber, error) {
	base, key, err := p.connect(get)
	if err != nil {
		return nil, err
	}
	return &whisper{provider: p.name, model: model, url: base + "/audio/transcriptions", key: key, timeout: speechTimeout}, nil
}

// whisper is one speech model at an OpenAI-style service.
type whisper struct {
	provider, model, url, key string
	timeout                   time.Duration
	// plain is set once the model has refused "verbose_json" (some newer speech models take only
	// "json"). Their answer has no length, so the cost then uses the length Seyal measured.
	plain atomic.Bool
}

// whisperReply is the "verbose_json" answer: the words, plus the language heard and the recording's
// length (used for the cost). Services that leave out the extras, or the plain "json" answer, still
// give "text".
type whisperReply struct {
	Text     string  `json:"text"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
}

// fileNames gives the services a file name they recognise; they look at its extension.
var fileNames = map[string]string{
	"audio/webm": "voice.webm", "audio/ogg": "voice.ogg", "audio/mp4": "voice.m4a", "audio/x-m4a": "voice.m4a",
	"audio/mpeg": "voice.mp3", "audio/wav": "voice.wav", "audio/aac": "voice.aac",
}

func (w *whisper) Transcribe(ctx context.Context, a Audio) (Transcript, error) {
	name, ok := fileNames[a.ContentType]
	if !ok {
		return Transcript{}, fmt.Errorf("%s: recordings of type %q can't be sent", w.provider, a.ContentType)
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout) // one limit for both tries below
	defer cancel()
	format := "verbose_json"
	if w.plain.Load() {
		format = "json"
	}
	status, raw, err := w.post(ctx, a, name, format)
	if err == nil && status == http.StatusBadRequest && format == "verbose_json" && strings.Contains(string(raw), "response_format") {
		w.plain.Store(true) // the model takes only plain "json": ask again, and from now on
		status, raw, err = w.post(ctx, a, name, "json")
	}
	if err != nil {
		return Transcript{}, err
	}
	if status != http.StatusOK {
		return Transcript{}, fmt.Errorf("%s answered %d %s: %s", w.provider, status, http.StatusText(status), serviceSaid(raw))
	}
	var reply whisperReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return Transcript{}, fmt.Errorf("%s: the answer can't be read: %w", w.provider, err)
	}
	return Transcript{Text: reply.Text, Language: reply.Language, Seconds: reply.Duration}, nil
}

// post sends the recording as a multipart form asking for the answer in format, and returns the
// service's status and answer (at most 1 MB of it).
func (w *whisper) post(ctx context.Context, a Audio, name, format string) (int, []byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
	h.Set("Content-Type", a.ContentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return 0, nil, err
	}
	part.Write(a.Data) // writes to memory; can't fail
	fields := [][2]string{{"model", w.model}, {"response_format", format}, {"temperature", "0"}}
	if a.Language != "" {
		fields = append(fields, [2]string{"language", a.Language})
	}
	if a.Words != "" {
		fields = append(fields, [2]string{"prompt", a.Words})
	}
	for _, f := range fields {
		mw.WriteField(f[0], f[1])
	}
	if err := mw.Close(); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, &body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if w.key != "" {
		req.Header.Set("Authorization", "Bearer "+w.key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", w.provider, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%s: reading the answer: %w", w.provider, err)
	}
	return res.StatusCode, raw, nil
}
