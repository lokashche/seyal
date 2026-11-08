// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import "testing"

// TestShareDigest: the fingerprint of the words Seyal offered matches the same words as a browser's
// text box gives them back (line ends as "\n"), and differs for any real change.
func TestShareDigest(t *testing.T) {
	offered := "Dear Acme Traders,\r\nplease send the papers.\rThanks"
	if ShareDigest(offered) != ShareDigest("Dear Acme Traders,\nplease send the papers.\nThanks") {
		t.Error("the same words with other line ends read as changed")
	}
	if ShareDigest(offered) == ShareDigest("Dear Acme Traders,\nplease send the papers.\nThanks!") {
		t.Error("changed words read as the same")
	}
	if len(ShareDigest("")) != 64 {
		t.Error("the fingerprint isn't SHA-256 in hex")
	}
}
