// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "errors"

// chatLinks reads the chat link switch (SEYAL_SHARE_CHAT_LINKS, D92): "on" shows the Share panel's
// WhatsApp button; empty or "off" (the default) hides it. Such a link may open the chat app's website,
// which then sees the words in the address, so it stays off until the maintainer agrees. Anything else
// stops the start with a clear message.
func chatLinks(getenv func(string) string) (bool, error) {
	switch getenv("SEYAL_SHARE_CHAT_LINKS") {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, errors.New(`SEYAL_SHARE_CHAT_LINKS must be "on" or "off"`)
}
