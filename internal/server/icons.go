// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import "html/template"

// icons are small line drawings (16×16, drawn in the text colour) for the folder column. They are
// drawn here rather than taken from an icon set, so there is nothing to credit or keep up to date.
var icons = map[string]string{
	"approval": `<circle cx="8" cy="8" r="5.75"/><path d="M5.6 8.2l1.6 1.6 3.2-3.5"/>`,
	"overdue":  `<circle cx="8" cy="8" r="5.75"/><path d="M8 5v3.3l2.1 1.3"/>`,
	"turn":     `<path d="M2.75 8h9M8.75 4.75 12 8l-3.25 3.25"/>`,
	"team":     `<circle cx="6" cy="6" r="2.1"/><path d="M2.5 13c.4-2.1 1.8-3.3 3.5-3.3s3.1 1.2 3.5 3.3"/><circle cx="11.2" cy="6.4" r="1.7"/><path d="M10.9 9.7c1.5.1 2.4 1.1 2.7 2.8"/>`,
	"waiting":  `<path d="M4.5 2.5h7M4.5 13.5h7M5.25 2.5c0 3 2.75 3.4 2.75 5.5s-2.75 2.5-2.75 5.5M10.75 2.5c0 3-2.75 3.4-2.75 5.5s2.75 2.5 2.75 5.5"/>`,
	"all":      `<path d="M2.5 9.25 4.1 4.3c.15-.5.6-.8 1.1-.8h5.6c.5 0 .95.3 1.1.8l1.6 4.95v3.25a1 1 0 0 1-1 1h-9a1 1 0 0 1-1-1z"/><path d="M2.5 9.25h3l.75 1.5h3.5l.75-1.5h3"/>`,
	"plus":     `<path d="M8 3.25v9.5M3.25 8h9.5"/>`,
	"settings": `<path d="M2.75 5h4.5M10.75 5h2.5M2.75 11h2.5M8.75 11h4.5"/><circle cx="9" cy="5" r="1.75"/><circle cx="7" cy="11" r="1.75"/>`,
	"logout":   `<path d="M6.5 2.75H4a1 1 0 0 0-1 1v8.5a1 1 0 0 0 1 1h2.5M10 5.25 12.75 8 10 10.75M12.75 8H6.25"/>`,
	"back":     `<path d="M12.75 8h-9.5M7 4.25 3.25 8 7 11.75"/>`,
}

// icon returns the named icon as inline SVG, hidden from screen readers (the text beside it says
// the same); an unknown name gives nothing.
func icon(name string) template.HTML {
	paths, ok := icons[name]
	if !ok {
		return ""
	}
	return template.HTML(`<svg class="ic" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" ` +
		`stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + paths + `</svg>`)
}
