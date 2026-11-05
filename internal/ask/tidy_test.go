// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"seyal/internal/ai"
)

func TestKeepsFacts(t *testing.T) {
	for _, c := range []struct {
		name, raw, tidied string
		ok                bool
	}{
		{"fillers and a repeat go", "umm Acme Traders, ah, Acme Traders called about the 15th", "Acme Traders called about the 15th.", true},
		{"a capital at the start of a sentence", "acme traders called. they want monthly books", "Acme traders called. They want monthly books.", true},
		{"Tamil and Tanglish stay", "அது வந்து Sundaram-க்கு monthly books start பண்ணுங்க", "Sundaram-க்கு monthly books start பண்ணுங்க.", true},
		{"a number added", "Acme Traders called", "Acme Traders called 2 times.", false},
		{"a number changed", "the fee is fifteen thousand", "the fee is 15000", false},
		{"a number split differently", "send 15000 today", "send 15,000 today", false},
		{"a name added", "the books are ready", "The books for Ganga Traders are ready.", false},
		{"empty", "Acme Traders called", "", false},
		{"much longer", "ok", "Ok, and here is a lot more that nobody said at all.", false},
		{"amounts swapped", "fee 500 advance 5000", "Fee 5000, advance 500.", false},
		{"an amount dropped", "fee 500 advance 5000", "Fee 500, advance.", false},
		{"an amount said twice in the answer", "fee 500 advance 5000", "Fee 500, advance 500 5000.", false},
		{"a not dropped", "advance not needed", "Advance needed.", false},
		{"a don't dropped", "don't send the books", "Send the books.", false},
		{"a Tamil not dropped", "advance இல்லை", "Advance.", false},
		{"a name dropped", "tell Ganga and Acme", "Tell Acme.", false},
		{"names swapped", "Ganga pays Acme", "Acme pays Ganga.", false},
		{"a number said twice by mistake goes", "fee 500 500 today", "Fee 500 today.", true},
		{"a phrase said twice by mistake goes", "Um, the fee, the fee is 500 for Acme Traders, Acme Traders", "The fee is 500 for Acme Traders.", true},
		{"the same facts, tidied", "fee 500 not 5000 for Acme", "Fee 500, not 5000, for Acme.", true},
	} {
		err := keepsFacts(c.raw, c.tidied)
		if (err == nil) != c.ok {
			t.Errorf("%s: keepsFacts = %v, want ok %v", c.name, err, c.ok)
		}
		// The reason is kept in AI usage for good, also when the note is discarded: it must be one
		// of the fixed reasons, never quoting a word of the note or of the answer.
		if err != nil && !slices.Contains(tidyRefusals, err) {
			t.Errorf("%s: keepsFacts = %q, want one of the fixed reasons", c.name, err)
		}
	}
}

// tidyRefusals are the fixed reasons a tidy-up's answer is refused for.
var tidyRefusals = []error{errTidyEmpty, errTidyLonger, errTidyNumber, errTidyName, errTidyMoved, errTidyLeftOut}

// chatService pretends to be an OpenAI-style chat service on this computer (no real service is
// called). It answers each request with content, after wait.
func chatService(t *testing.T, content string, wait time.Duration, got *[]map[string]any) *ai.Setup {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		*got = append(*got, body)
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		reply, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
			"usage":   map[string]any{"prompt_tokens": 300, "completion_tokens": 20},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(reply)
	}))
	t.Cleanup(srv.Close)
	setup, err := ai.FromEnv(func(k string) string {
		return map[string]string{"SEYAL_AI_TIDY": "compat:tidy-model", "SEYAL_COMPAT_BASE_URL": srv.URL + "/v1"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	return setup
}

func TestTidy(t *testing.T) {
	ctx := context.Background()
	raw := "umm Acme Traders, ah, Acme Traders wants monthly books"
	link := ai.Link{PersonID: "p1"}

	t.Run("off: nothing to tidy with", func(t *testing.T) {
		if tidied, call := Tidy(ctx, &ai.Setup{}, raw, link); tidied != "" || call != nil {
			t.Errorf("Tidy with AI off = %q, %+v", tidied, call)
		}
	})

	t.Run("the tidied words, and the call to record", func(t *testing.T) {
		var got []map[string]any
		setup := chatService(t, `{"text":"Acme Traders wants monthly books."}`, 0, &got)
		tidied, call := Tidy(ctx, setup, raw, link)
		if tidied != "Acme Traders wants monthly books." || call == nil || call.Err != nil ||
			call.Purpose != ai.Tidy || call.Model != "compat:tidy-model" || call.Usage.Output != 20 {
			t.Fatalf("Tidy = %q, %+v", tidied, call)
		}
		if len(got) != 1 || !strings.Contains(asJSON(got[0]), "Never add, remove or change facts") ||
			!strings.Contains(asJSON(got[0]), raw) {
			t.Errorf("the service got %v", got)
		}
	})

	t.Run("an answer with a new name is refused, and recorded as failed", func(t *testing.T) {
		var got []map[string]any
		setup := chatService(t, `{"text":"Acme Traders and Ganga Traders want monthly books."}`, 0, &got)
		tidied, call := Tidy(ctx, setup, raw, link)
		if tidied != "" || call == nil || call.Err != errTidyName || strings.Contains(call.Err.Error(), "Ganga") {
			t.Errorf("Tidy = %q, %+v", tidied, call)
		}
	})

	t.Run("too slow: given up, the words as heard are used", func(t *testing.T) {
		defer func(d time.Duration) { tidyTimeout = d }(tidyTimeout)
		tidyTimeout = 100 * time.Millisecond
		var got []map[string]any
		setup := chatService(t, `{"text":"Acme Traders wants monthly books."}`, 3*time.Second, &got)
		start := time.Now()
		tidied, call := Tidy(ctx, setup, raw, link)
		if tidied != "" || call == nil || call.Err == nil || time.Since(start) > 2*time.Second {
			t.Errorf("slow Tidy = %q, %+v after %v", tidied, call, time.Since(start))
		}
	})

	t.Run("a note too long to tidy: no call, the words as heard are used", func(t *testing.T) {
		var got []map[string]any
		setup := chatService(t, `{"text":"Acme Traders wants monthly books."}`, 0, &got)
		long := strings.Repeat("அ", tidyMaxLetters+1)
		if tidied, call := Tidy(ctx, setup, long, link); tidied != "" || call != nil || len(got) != 0 {
			t.Errorf("Tidy of a long note = %q, %+v, %d calls", tidied, call, len(got))
		}
		if tidyTime(0) != tidyTimeout || tidyTime(tidyMaxLetters) != tidyTimeout+20*time.Second {
			t.Errorf("tidyTime = %v, %v", tidyTime(0), tidyTime(tidyMaxLetters))
		}
	})

	t.Run("an answer that isn't JSON is refused", func(t *testing.T) {
		var got []map[string]any
		setup := chatService(t, `Acme Traders wants monthly books.`, 0, &got)
		if tidied, call := Tidy(ctx, setup, raw, link); tidied != "" || call == nil || call.Err != errTidyUnreadable {
			t.Errorf("Tidy = %q, %+v", tidied, call)
		}
	})
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
