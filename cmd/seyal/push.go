// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"

	"seyal/internal/push"
)

// runPush runs "seyal push keys": it makes the server's phone-note keys (VAPID) and prints them as
// two .env lines. Nothing leaves the computer and nothing is saved: copy the lines into the
// server's .env yourself, and back the private key up with the other secrets.
func runPush(args []string, out io.Writer) error {
	if len(args) != 1 || args[0] != "keys" {
		return errors.New(`usage: seyal push keys`)
	}
	keys, err := push.GenerateKeys()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "SEYAL_PUSH_PUBLIC_KEY=%s\nSEYAL_PUSH_PRIVATE_KEY=%s\n", keys.Public(), keys.Private())
	return nil
}

// pushKey reads the phone-note switch (SEYAL_PUSH) and returns the server's public push key and the
// sender that posts phone notes when it is "on", or "" and nil when it is off (empty or "off", the
// default). On means the keys and the contact must be right: a mistake stops the start with a clear
// message, which never shows a key. Nothing is sent anywhere here.
func pushKey(getenv func(string) string) (string, *push.Sender, error) {
	switch getenv("SEYAL_PUSH") {
	case "", "off":
		return "", nil, nil
	case "on":
	default:
		return "", nil, errors.New(`SEYAL_PUSH must be "on" or "off"`)
	}
	keys, err := push.ParseKeys(getenv("SEYAL_PUSH_PUBLIC_KEY"), getenv("SEYAL_PUSH_PRIVATE_KEY"))
	if err != nil {
		return "", nil, fmt.Errorf("SEYAL_PUSH=on: %w (make them with `seyal push keys`)", err)
	}
	sender, err := push.NewSender(keys, getenv("SEYAL_PUSH_CONTACT"))
	if err != nil {
		return "", nil, fmt.Errorf("SEYAL_PUSH=on: %w", err)
	}
	return keys.Public(), sender, nil
}
