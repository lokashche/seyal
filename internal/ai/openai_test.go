// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeService pretends to be an OpenAI-style service on this computer. It records what it was sent
// and answers with reply, or refuses JSON schemas when refuseSchema is set.
type fakeService struct {
	got          []map[string]any
	auth         string
	refuseSchema bool
	missOnce     bool // reject the first schema answer as not fitting, like Groq's json_validate_failed
	status       int
	reply        string
}

func (f *fakeService) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.auth = r.Header.Get("Authorization")
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		f.got = append(f.got, body)
		rf, _ := body["response_format"].(map[string]any)
		switch {
		case f.missOnce && rf["type"] == "json_schema":
			f.missOnce = false
			http.Error(w, `{"error":{"message":"Failed to validate JSON.","code":"json_validate_failed","failed_generation":""}}`, http.StatusBadRequest)
		case f.refuseSchema && rf["type"] == "json_schema":
			http.Error(w, `{"error":{"message":"response_format json_schema is not supported by this model"}}`, http.StatusBadRequest)
		case f.status != 0:
			http.Error(w, `{"error":{"message":"slow down"}}`, f.status)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, f.reply)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

const goodReply = `{"choices":[{"message":{"role":"assistant","content":"{\"kind\":\"note\"}"}}],
	"usage":{"prompt_tokens":1200,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":1000}}}`

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestChatSendsAndReads(t *testing.T) {
	f := &fakeService{reply: goodReply}
	s, err := FromEnv(env(map[string]string{
		"SEYAL_AI_UNDERSTAND": "groq:some-model", "SEYAL_GROQ_API_KEY": "test-key", "SEYAL_GROQ_BASE_URL": f.start(t),
	}))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.For(Understand)
	resp, err := c.Model.Complete(context.Background(), Request{Instructions: "rules", Input: "hello",
		Schema: map[string]any{"type": "object"}, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"kind":"note"}` || resp.Usage != (Usage{Input: 200, Cached: 1000, Output: 30}) {
		t.Errorf("response = %+v", resp)
	}
	if f.auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", f.auth)
	}
	got := f.got[0]
	msgs, _ := got["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	rf, _ := got["response_format"].(map[string]any)
	if got["model"] != "some-model" || len(msgs) != 2 || first["role"] != "system" || first["content"] != "rules" ||
		rf["type"] != "json_schema" || got["max_completion_tokens"] != 100.0 {
		t.Errorf("request = %v", got)
	}
}

// TestChatFallsBackToPlainJSON: a model that refuses JSON schemas is asked for plain JSON, with the
// schema in the instructions, and isn't asked with a schema again.
func TestChatFallsBackToPlainJSON(t *testing.T) {
	f := &fakeService{reply: goodReply, refuseSchema: true}
	m, err := newChat(preset{name: "compat"}, "local-model", env(map[string]string{"SEYAL_COMPAT_BASE_URL": f.start(t)}))
	if err != nil {
		t.Fatal(err)
	}
	r := Request{Instructions: "rules", Input: "hi", Schema: map[string]any{"type": "object", "title": "proposal"}, MaxTokens: 50}
	for range 2 {
		if _, err := m.Complete(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.got) != 3 {
		t.Fatalf("%d requests, want 3 (schema refused once, then plain JSON twice)", len(f.got))
	}
	plain := f.got[1]
	rf, _ := plain["response_format"].(map[string]any)
	msgs, _ := plain["messages"].([]any)
	system, _ := msgs[0].(map[string]any)
	if rf["type"] != "json_object" || !strings.Contains(system["content"].(string), `"title":"proposal"`) ||
		plain["max_tokens"] != 50.0 || f.auth != "" {
		t.Errorf("plain JSON request = %v (auth %q)", plain, f.auth)
	}
}

// TestChatRetriesAMissedAnswer: one answer that didn't fit the schema is retried in plain JSON, and the
// next request uses the schema again.
func TestChatRetriesAMissedAnswer(t *testing.T) {
	f := &fakeService{reply: goodReply, missOnce: true}
	m, err := newChat(presets[0], "m", env(map[string]string{"SEYAL_GROQ_API_KEY": "k", "SEYAL_GROQ_BASE_URL": f.start(t)}))
	if err != nil {
		t.Fatal(err)
	}
	r := Request{Input: "hi", Schema: map[string]any{"type": "object"}}
	for range 2 {
		if _, err := m.Complete(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	var kinds []any
	for _, g := range f.got {
		kinds = append(kinds, g["response_format"].(map[string]any)["type"])
	}
	if len(kinds) != 3 || kinds[0] != "json_schema" || kinds[1] != "json_object" || kinds[2] != "json_schema" {
		t.Errorf("response formats = %v, want json_schema, json_object, json_schema", kinds)
	}
}

func TestChatErrors(t *testing.T) {
	f := &fakeService{status: http.StatusTooManyRequests}
	m, err := newChat(presets[0], "m", env(map[string]string{"SEYAL_GROQ_API_KEY": "secret-key", "SEYAL_GROQ_BASE_URL": f.start(t)}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Complete(context.Background(), Request{Input: "hi"})
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "secret-key") {
		t.Errorf("rate-limited call = %v; want the status, never the key", err)
	}

	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no key", map[string]string{"SEYAL_AI_DRAFT": "groq:m"}, "SEYAL_GROQ_API_KEY"},
		{"no address", map[string]string{"SEYAL_AI_DRAFT": "compat:m"}, "SEYAL_COMPAT_BASE_URL"},
	} {
		if _, err := FromEnv(env(tt.env)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: FromEnv = %v, want a problem mentioning %s", tt.name, err, tt.want)
		}
	}
}
