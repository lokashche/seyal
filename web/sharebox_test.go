// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shareboxScript runs the pure part of static/sharebox.js in Node (D93): it builds a waiting share from
// what a Share menu posted, decides what pages do with it, and what a Send's answer does to it. It prints
// the results as JSON for the test to check. It runs the page's own code.
const shareboxScript = `
const B = require(process.argv[1]);
const now = 1794000000000; // a fixed "now"
const kb = (n) => n * 1024;
const files = [
  { name: "PTT-20261111-WA0003.opus", type: "application/octet-stream", size: kb(40), data: "voice" },
  { name: "report.docx", type: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", size: kb(20), data: "doc" },
  { name: "statement.pdf", type: "application/pdf", size: kb(300), data: "pdf" },
  { name: "a.jpg", type: "image/jpeg", size: kb(10), data: "1" },
  { name: "b.jpg", type: "image/jpeg", size: kb(10), data: "2" },
  { name: "c.jpg", type: "image/jpeg", size: kb(10), data: "3" },
  { name: "d.jpg", type: "image/jpeg", size: kb(10), data: "4" },
];
const share = B.make({ title: "", text: "Statements come tomorrow https://acme.example/x", url: "https://acme.example/x" },
  files, now, "", "0b6c3f9e-2d4a-4c1b-9e8f-7a6b5c4d3e2f");
const big = B.make({ text: "" }, [{ name: "huge.pdf", type: "application/pdf", size: 26 * 1024 * 1024, data: "x" }], now, "asha", "id2");
const long = B.make({ text: "அ".repeat(10005) }, [], now, "asha", "id3");
const owned = Object.assign({}, share, { owner: "asha" });
console.log(JSON.stringify({
  share: { text: share.text, owner: share.owner, files: share.files.map((f) => f.name + "|" + f.type), left: share.left },
  big: big.left, long: { cut: long.cut, letters: Array.from(long.text).length },
  decide: {
    none: B.decide(null, "asha", now),
    nobodyYet: B.decide(share, "asha", now),
    same: B.decide(owned, "asha", now + 1000),
    other: B.decide(owned, "ben", now + 1000),
    loginPage: B.decide(owned, "", now + 1000),
    dayOld: B.decide(owned, "asha", now + B.LIFE),
    clockBack: B.decide(owned, "asha", now - 2 * B.LIFE),
  },
  after: {
    ok: B.afterSend(200, "/t/kaveri-demo/items/7f3c"),
    pickProcess: B.afterSend(200, "/t/kaveri-demo/tell"),
    refused: B.afterSend(422, "/t/kaveri-demo/tell"),
    closed: B.afterSend(409, "/t/kaveri-demo/tell"),
    sharingOff: B.afterSend(403, "/t/kaveri-demo/tell"),
    serverDown: B.afterSend(502, "/t/kaveri-demo/tell"),
    noAnswer: B.afterSend(0, ""),
    loggedOut: B.afterSend(200, "/t/kaveri-demo/login"),
    newPassword: B.afterSend(200, "/t/kaveri-demo/password"),
    notHere: B.afterSend(200, "/"),
  },
  mayTry: {
    fresh: B.mayTry(owned, "kaveri-demo"),
    sameBusiness: B.mayTry(Object.assign({}, owned, { tried: "kaveri-demo" }), "kaveri-demo"),
    otherBusiness: B.mayTry(Object.assign({}, owned, { tried: "kaveri-demo" }), "acme-traders"),
  },
}));
`

// TestWaitingShare: what is shared into Seyal is kept on the phone with caps (5 files, 25 MB, 10,000
// letters, only the kinds Seyal keeps, a WhatsApp voice note's type fixed from its name), shown only to
// the login it belongs to (another login's is cleared, a share nobody owned yet goes to the first login
// to open it, one older than a day is cleared), and cleared only by the server's OK: a failed Send keeps
// it, and one with no answer goes only to the business it was sent to (D93). It needs Node; without it
// the test is skipped (the script was then not run).
func TestWaitingShare(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, so static/sharebox.js can't be run here")
	}
	script, err := filepath.Abs("static/sharebox.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", shareboxScript, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Share struct {
			Text, Owner string
			Files       []string
			Left        []struct{ Name, Why string }
		}
		Big  []struct{ Name, Why string }
		Long struct {
			Cut     bool
			Letters int
		}
		Decide map[string]string
		After  map[string]string
		MayTry map[string]bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node printed %s (%v)", out, err)
	}

	// The link Android puts in the text is not repeated from url.
	if got.Share.Text != "Statements come tomorrow https://acme.example/x" || got.Share.Owner != "" {
		t.Errorf("the share's words = %q, owner %q", got.Share.Text, got.Share.Owner)
	}
	wantFiles := []string{"PTT-20261111-WA0003.opus|audio/ogg", "statement.pdf|application/pdf", "a.jpg|image/jpeg",
		"b.jpg|image/jpeg", "c.jpg|image/jpeg"}
	if strings.Join(got.Share.Files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("kept files = %v, want %v", got.Share.Files, wantFiles)
	}
	if len(got.Share.Left) != 2 || got.Share.Left[0].Name != "report.docx" || got.Share.Left[0].Why != "type" ||
		got.Share.Left[1].Name != "d.jpg" || got.Share.Left[1].Why != "count" {
		t.Errorf("left out = %+v; want report.docx (kind) and d.jpg (over 5 files)", got.Share.Left)
	}
	if len(got.Big) != 1 || got.Big[0].Why != "size" {
		t.Errorf("a 26 MB file: left out %+v; want it left out for size", got.Big)
	}
	if !got.Long.Cut || got.Long.Letters != 10000 {
		t.Errorf("10,005 Tamil letters: cut %v to %d; want cut to 10,000", got.Long.Cut, got.Long.Letters)
	}

	for name, want := range map[string]string{
		"none": "none", "nobodyYet": "claim", "same": "show", "other": "clear", "loginPage": "keep",
		"dayOld": "clear", "clockBack": "clear",
	} {
		if got.Decide[name] != want {
			t.Errorf("decide %s = %q, want %q", name, got.Decide[name], want)
		}
	}
	for name, want := range map[string]string{
		"ok": "clear", "pickProcess": "clear", "refused": "keep", "closed": "keep", "sharingOff": "keep",
		"serverDown": "keep", "noAnswer": "keep", "loggedOut": "keep", "newPassword": "keep", "notHere": "keep",
	} {
		if got.After[name] != want {
			t.Errorf("after a Send (%s) = %q, want %q", name, got.After[name], want)
		}
	}
	// A Send whose answer never came may have reached its business: until that business answers, the
	// share goes only there, not to another business by [change] (one share, never in two ledgers).
	for name, want := range map[string]bool{"fresh": true, "sameBusiness": true, "otherBusiness": false} {
		if got.MayTry[name] != want {
			t.Errorf("may send (%s) = %v, want %v", name, got.MayTry[name], want)
		}
	}
}
