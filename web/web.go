// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web holds the page templates and static files, built into the program.
package web

import "embed"

// Files holds templates/ (html/template pages) and static/ (CSS, scripts, icons).
//
//go:embed templates static
var Files embed.FS
