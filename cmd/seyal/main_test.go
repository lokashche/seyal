// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr bool
		wantOut string
	}{
		{args: []string{"version"}, wantOut: appName + " " + version},
		{args: []string{"--version"}, wantOut: appName + " " + version},
		{args: []string{"help"}, wantOut: "Usage:"},
		{args: nil, wantErr: true, wantOut: "Usage:"},
		{args: []string{"nope"}, wantErr: true},
		{args: []string{"dev"}, wantErr: true},
		{args: []string{"dev", "nope"}, wantErr: true},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		err := run(tt.args, &out)
		if (err != nil) != tt.wantErr {
			t.Errorf("run(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
		}
		if !strings.Contains(out.String(), tt.wantOut) {
			t.Errorf("run(%q) output = %q, want it to contain %q", tt.args, out.String(), tt.wantOut)
		}
	}
}
