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
