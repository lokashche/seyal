// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"

	"seyal/internal/config"
	"seyal/migrations"
)

// runMigrate applies all pending database migrations as the owner role.
func runMigrate(out io.Writer) error {
	url, err := config.Need("SEYAL_OWNER_DATABASE_URL")
	if err != nil {
		return err
	}
	applied, err := migrations.Up(context.Background(), url)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if len(applied) == 0 {
		fmt.Fprintln(out, "database is up to date")
		return nil
	}
	for _, name := range applied {
		fmt.Fprintln(out, "applied", name)
	}
	return nil
}
