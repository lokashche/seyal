// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/channel"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// maxWords limits the word list sent with a voice note, in bytes. Speech services read only a short
// hint (Whisper: about 220 tokens); Tamil letters take several bytes, so bytes are the safer count.
const maxWords = 400

// VoiceWords lists names people are likely to say in a voice note, to help the speech service spell
// them: the entries of the business's lists that the process uses. In a work item's thread, these
// are the lists its process chooses from, with the entries the item already has first; on Tell Seyal
// (itemID ""), the lists of every process in use. Only entry names from the lists are sent, never
// people's names or the item's other details (D58), and only up to maxWords bytes.
// tx must come from db.WithTenant.
func VoiceWords(ctx context.Context, tx pgx.Tx, itemID string) (string, error) {
	var kinds, first []string
	if itemID != "" {
		it, err := workitem.Get(ctx, tx, itemID)
		if err != nil {
			return "", err
		}
		p, err := workitem.PlaybookFor(ctx, tx, it)
		if err != nil {
			return "", err
		}
		kinds = listKinds(p)
		for _, f := range p.Fields {
			if key, ok := it.Fields[f.Key].(string); ok && f.Type == "master" {
				var name string
				err := tx.QueryRow(ctx, `SELECT name FROM master_items WHERE tenant_id = current_tenant() AND kind = $1 AND key = $2`,
					f.Kind, key).Scan(&name)
				if err == nil {
					first = append(first, name)
				}
			}
		}
	} else {
		rows, _ := tx.Query(ctx, `
			SELECT pv.source FROM tenant_playbooks tp
			JOIN playbook_versions pv ON pv.tenant_id = tp.tenant_id AND pv.id = tp.current_version_id
			WHERE tp.tenant_id = current_tenant() AND tp.active ORDER BY tp.playbook_id`)
		sources, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return "", err
		}
		for _, src := range sources {
			p, err := playbook.Parse([]byte(src))
			if err != nil {
				return "", err
			}
			for _, k := range listKinds(p) {
				if !slices.Contains(kinds, k) {
					kinds = append(kinds, k)
				}
			}
		}
	}
	if len(kinds) == 0 && len(first) == 0 {
		return "", nil
	}
	rows, _ := tx.Query(ctx, `
		SELECT name FROM master_items WHERE tenant_id = current_tenant() AND active AND kind = ANY($1)
		ORDER BY name LIMIT 200`, kinds)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var seen []string
	for _, n := range append(first, names...) {
		n = strings.Join(strings.Fields(n), " ")
		if n == "" || slices.Contains(seen, n) {
			continue
		}
		if b.Len()+len(n)+2 > maxWords {
			break
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(n)
		seen = append(seen, n)
	}
	return b.String(), nil
}

// listKinds returns the lists a process chooses from, in the order of its fields.
func listKinds(p *playbook.Playbook) []string {
	var kinds []string
	for _, f := range p.Fields {
		if (f.Type == "master" || f.Type == "list" && f.Of == "master") && f.Kind != "" && !slices.Contains(kinds, f.Kind) {
			kinds = append(kinds, f.Kind)
		}
	}
	return kinds
}

// wordsOnly reports whether a message is only words: it carries no files, apart from the voice note
// its words were spoken in (speak instead of type). So a spoken greeting or question is answered like
// a typed one. A voice note sent without words is not words only.
func wordsOnly(msg channel.Message) bool {
	for _, md := range msg.Media {
		if msg.Transcript == nil || md.ID != msg.Transcript.MediaID || strings.TrimSpace(msg.Text) == "" {
			return false
		}
	}
	return true
}
