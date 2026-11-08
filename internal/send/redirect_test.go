// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// twoFakes starts a second fake and then a first one (both on this computer, on two ports). first
// gets the second's base address.
func twoFakes(t *testing.T, first func(second string) http.HandlerFunc, second http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	b := httptest.NewTLSServer(second)
	t.Cleanup(b.Close)
	return fake(t, first(b.URL))
}

func TestPostRedirectFollowsOneAllowedRedirect(t *testing.T) {
	type seen struct{ method, body, sig string }
	var got []seen
	srv, address := twoFakes(t,
		func(second string) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				code := http.StatusFound
				if r.URL.Query().Get("keep") == "1" {
					code = http.StatusTemporaryRedirect
				}
				http.Redirect(w, r, second+"/echo?user_content_key="+marker, code)
			}
		},
		func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = append(got, seen{r.Method, string(b), r.Header.Get("webhook-signature")})
			w.Write([]byte(`{"ok":true}`))
		})
	c := testCaller(srv, 5*time.Second, 100)
	h := http.Header{"Webhook-Signature": {"v1,abc"}}

	// 302: the second call is a plain GET, with no body and no signature.
	ans, err := c.PostRedirect(context.Background(), address, []string{"127.0.0.1"}, h, []byte(`{"a":1}`))
	if err != nil || ans.Status != 200 || string(ans.Body) != `{"ok":true}` {
		t.Fatalf("302 then 200: %+v, %v", ans, err)
	}
	// 307: the body and headers go again.
	ans, err = c.PostRedirect(context.Background(), address+"?keep=1", []string{"127.0.0.1"}, h, []byte(`{"a":1}`))
	if err != nil || ans.Status != 200 {
		t.Fatalf("307 then 200: %+v, %v", ans, err)
	}
	want := []seen{{"GET", "", ""}, {"POST", `{"a":1}`, "v1,abc"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the second fake saw %+v, want %+v", got, want)
	}
}

func TestPostRedirectRefusesOtherRedirects(t *testing.T) {
	reached := false
	var srv *httptest.Server
	srv, address := twoFakes(t,
		func(second string) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				to := map[string]string{
					"other": "https://accounts.google.example/signin?continue=" + marker,
					"http":  strings.Replace(second, "https://", "http://", 1) + "/" + marker,
					"twice": srv.URL + "/" + marker + "/exec?bounce=1",
					"plain": second + "/" + marker,
				}[q.Get("to")]
				if q.Get("bounce") == "1" {
					to = "https://accounts.google.com/ServiceLogin?continue=" + marker
				}
				http.Redirect(w, r, to, http.StatusFound)
			}
		},
		func(http.ResponseWriter, *http.Request) { reached = true })
	c := testCaller(srv, 5*time.Second, 100)
	c.hostOK = func(h string) bool { return h == "127.0.0.1" || strings.HasPrefix(h, "accounts.google.") }

	for to, wantHost := range map[string]string{
		"other": "accounts.google.example", // not in redirect_to
		"http":  "127.0.0.1",               // not https
		"twice": "accounts.google.com",     // a second redirect is never followed
	} {
		ans, err := c.PostRedirect(context.Background(), address+"?to="+to, []string{"127.0.0.1"}, nil, nil)
		if err != nil || !isRedirect(ans.Status) || ans.RedirectHost != wantHost || len(ans.Body) != 0 {
			t.Errorf("%s: %+v, %v; want the 302 itself, host %s, no body", to, ans, err, wantHost)
		}
	}
	// Post (no redirect_to) never follows, not even to an allowed host.
	if ans, err := c.Post(context.Background(), address+"?to=plain", nil, nil); err != nil || ans.Status != http.StatusFound {
		t.Errorf("Post followed a redirect: %+v, %v", ans, err)
	}
	if reached {
		t.Error("the second fake was reached")
	}
	if v := Judge(Answer{Status: 302, RedirectHost: "accounts.google.com"}, nil, true); v.Reason != ReasonSignIn {
		t.Errorf("a redirect to the sign-in page = %+v", v)
	}
}

// A redirect to an address inside this computer is refused by the real dial check, even when the
// host is in redirect_to and on the host list.
func TestPostRedirectToAPrivateAddressIsRefused(t *testing.T) {
	reached := false
	srv, address := twoFakes(t,
		func(second string) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, strings.Replace(second, "127.0.0.1", "localhost", 1)+"/"+marker, http.StatusFound)
			}
		},
		func(http.ResponseWriter, *http.Request) { reached = true })
	firstHop := strings.TrimPrefix(srv.URL, "https://")
	// The test may reach the first fake; every other connection gets the real check.
	dial := func(network, address string, rc syscall.RawConn) error {
		if address == firstHop {
			return nil
		}
		return checkDial(network, address, rc)
	}
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	c := newCaller(func(h string) bool { return h == "127.0.0.1" || h == "localhost" }, 5*time.Second, 100, dial, cfg)
	c.anyPort = true
	_, err := c.PostRedirect(context.Background(), address, []string{"localhost"}, nil, nil)
	var f *Failure
	if !errors.As(err, &f) || f.Reason != ReasonBlocked {
		t.Errorf("a redirect to localhost: %v, want %q", err, ReasonBlocked)
	}
	if reached {
		t.Error("the fake on this computer was reached through a redirect")
	}
}

// A marker word in the fake's address (and in the redirect it answers with, and in its replies) must
// never show up in anything Seyal could store or log: an error, an answer's host or Retry-After, a
// verdict's reason, or a log line. (The ledger and the thread get only these reasons; part 4c-3 repeats
// this check through them.)
func TestTheAddressNeverShows(t *testing.T) {
	var logs bytes.Buffer
	oldLog, oldSlog := log.Writer(), slog.Default()
	log.SetOutput(&logs)
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { log.SetOutput(oldLog); slog.SetDefault(oldSlog) })

	block := make(chan struct{})
	srv, address := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("do") {
		case "slow":
			<-block
		case "503":
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("down: " + marker))
		case "redirect":
			http.Redirect(w, r, "https://accounts.google.com/x?continue="+r.URL.String(), http.StatusFound)
		case "notok":
			w.Write([]byte(`{"ok":false,"why":"no tab called Sheet1"}`))
		case "html":
			w.Write([]byte("<html>" + marker + "</html>"))
		}
	})
	t.Cleanup(func() { close(block) }) // runs before srv.Close, so the slow handler can end
	c := testCaller(srv, 300*time.Millisecond, 100)
	untrusted := newCaller(c.hostOK, time.Second, 100, allowAll, nil) // doesn't trust the fake's certificate
	untrusted.anyPort = true
	blocked := newCaller(c.hostOK, time.Second, 100, checkDial, nil) // the real dial check
	blocked.anyPort = true

	var stored []string
	keep := func(ans Answer, err error) {
		if err != nil {
			stored = append(stored, err.Error())
		}
		stored = append(stored, ans.RedirectHost, ans.RetryAfter, Judge(ans, err, true).Reason, Judge(ans, err, false).Reason)
	}
	for _, do := range []string{"slow", "503", "redirect", "notok", "html"} {
		keep(c.PostRedirect(context.Background(), address+"?do="+do, []string{"script.googleusercontent.example"}, nil, []byte(`{}`)))
	}
	keep(untrusted.PostRedirect(context.Background(), address, nil, nil, nil))
	keep(blocked.PostRedirect(context.Background(), address, nil, nil, nil))
	keep(c.PostRedirect(context.Background(), strings.Replace(address, "https", "http", 1), nil, nil, nil))
	keep(c.PostRedirect(context.Background(), address, nil, http.Header{"X": {"a\n" + marker}}, nil))
	keep(NewAppCaller(Hosts{}).PostRedirect(context.Background(), address, nil, nil, nil))

	all := strings.Join(stored, "\n")
	if strings.Contains(all, marker) || strings.Contains(logs.String(), marker) {
		t.Errorf("the marker shows:\n%s\nlogs: %s", all, logs.String())
	}
	for _, want := range []string{ReasonTimeout, "answered 503", ReasonSignIn, "script failed: no tab called Sheet1",
		ReasonUnclear, ReasonTLS, ReasonBlocked, ReasonNotHTTPS, ReasonBadHeader, ReasonHost} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q among the reasons:\n%s", want, all)
		}
	}
}
