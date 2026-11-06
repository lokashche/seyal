// SPDX-License-Identifier: AGPL-3.0-or-later

package push

import (
	"strings"
	"testing"
)

func TestPushHosts(t *testing.T) {
	allowed := []string{
		"https://fcm.googleapis.com/fcm/send/fake0000:APA91bFake0000",
		"https://fcm.googleapis.com/wp/fake0000",
		"https://FCM.googleapis.com/wp/fake0000", // host names don't care about case
		"https://fcm.googleapis.com:443/wp/fake0000",
		"https://updates.push.services.mozilla.com/wpush/v2/gAAAAfake0000",
		"https://web.push.apple.com/QFake0000",
		"https://wns2-par02p.notify.windows.com/w/?token=BQYAAAfake0000",
	}
	for _, e := range allowed {
		if err := CheckEndpoint(e); err != nil {
			t.Errorf("CheckEndpoint(%q) = %v, want allowed", e, err)
		}
	}

	refused := []string{
		"",
		"fcm.googleapis.com/wp/fake0000",        // no scheme
		"http://fcm.googleapis.com/wp/fake0000", // not https
		"https://fcm.googleapis.com.evil.example/wp/x", // a look-alike in front of another host
		"https://evil.example/fcm.googleapis.com",      // the push host only in the path
		"https://evil.example/?u=https://fcm.googleapis.com",
		"https://evilpush.apple.com/x",  // not a name under push.apple.com
		"https://push.apple.com/x",      // the bare domain is not a push host
		"https://.push.apple.com/x",     // an empty name in front
		"https://a..push.apple.com/x",   // an empty label
		"https://web.push.apple.com./x", // a trailing dot
		"https://notify.windows.com.evil.example/x",
		"https://googleapis.com/x",
		"https://android.googleapis.com/gcm/send/x", // Google's old address: not on the list
		"https://user:pass@fcm.googleapis.com/wp/x", // a name and password in the address
		"https://fcm.googleapis.com:8443/wp/x",      // another port
		"https://127.0.0.1/wp/x",                    // an IP address, not a name
		"https://[::1]/wp/x",
		"https://169.254.169.254/latest/meta-data/", // the cloud "metadata" address
		"https://metadata.google.internal/computeMetadata/v1/",
		"https://localhost/wp/x",
		"https://fcm%2egoogleapis.com/wp/x", // an escaped dot
		"javascript:alert(1)",
		"https://fcm.googleapis.com/" + strings.Repeat("a", 2048), // too long
	}
	for _, e := range refused {
		err := CheckEndpoint(e)
		if err == nil {
			t.Errorf("CheckEndpoint(%q) allowed, want refused", e)
			continue
		}
		if e != "" && strings.Contains(err.Error(), e) {
			t.Errorf("CheckEndpoint(%q): the error shows the address: %v", e, err)
		}
	}
}
