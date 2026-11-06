// SPDX-License-Identifier: AGPL-3.0-or-later

package push

import (
	"strings"

	"seyal/internal/send"
)

// The push services of the four browser makers. A browser hands Seyal its device push address, so
// Seyal posts only to these hosts, never to an address a browser (or an attacker) made up. They are
// fixed values here, not settings: a new push service means a code change and a review.
const fcmHost = "fcm.googleapis.com" // Google: Chrome, Android

// pushDomains are matched by their end: a host must be a name *under* one of them.
var pushDomains = []string{
	".push.services.mozilla.com", // Mozilla: Firefox
	".push.apple.com",            // Apple: Safari, iPhone
	".notify.windows.com",        // Microsoft: Edge on Windows
}

// IsPushHost reports whether host (in lower case, without a port) is one of the push services.
func IsPushHost(host string) bool {
	if host == fcmHost {
		return true
	}
	for _, d := range pushDomains {
		// The part in front must be a real name: "web.push.apple.com", not ".push.apple.com".
		if name, ok := strings.CutSuffix(host, d); ok && name != "" && !strings.HasSuffix(name, ".") {
			return true
		}
	}
	return false
}

// CheckEndpoint checks a device push address: https, on one of the push services, port 443 and
// not too long. The error never holds the address.
func CheckEndpoint(endpoint string) error {
	_, err := send.CheckAddress(endpoint, IsPushHost)
	return err
}
