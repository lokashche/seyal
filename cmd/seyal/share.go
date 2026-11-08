// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"

	"seyal/internal/send"
)

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

// sending reads the switch for other apps (SEYAL_SEND, D95) and the hosts the maintainer agreed
// (SEYAL_SEND_HOSTS). "on" lets Seyal send to the other apps a business's set-up file lists, but
// only to those hosts; empty or "off" (the default) sends nothing. A bad value stops the start with a
// clear message, even when sending is off, so a mistake shows before it matters.
func sending(getenv func(string) string) (bool, send.Hosts, error) {
	hosts, err := send.ParseHosts(getenv("SEYAL_SEND_HOSTS"))
	if err != nil {
		return false, nil, errors.New("SEYAL_SEND_HOSTS: " + err.Error())
	}
	switch getenv("SEYAL_SEND") {
	case "", "off":
		return false, hosts, nil
	case "on":
		return true, hosts, nil
	}
	return false, nil, errors.New(`SEYAL_SEND must be "on" or "off"`)
}
