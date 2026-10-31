// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config reads settings from environment variables, optionally loaded from a .env file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// LoadDotEnv reads KEY=value lines from path and sets each variable that is not already set.
// A missing file is not an error.
func LoadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s line %d: expected KEY=value", path, i+1)
		}
		key = strings.TrimSpace(key)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, strings.TrimSpace(value))
		}
	}
	return nil
}

// Need returns the value of an environment variable, or an error if it is empty.
func Need(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s is not set (see .env.example)", key)
	}
	return v, nil
}
