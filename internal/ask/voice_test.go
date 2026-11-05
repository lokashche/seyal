// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"testing"

	"seyal/internal/channel"
)

func TestWordsOnly(t *testing.T) {
	voice := channel.Media{ID: "v1", ContentType: "audio/webm"}
	photo := channel.Media{ID: "p1", ContentType: "image/png"}
	spoken := &channel.Transcript{ID: "t1", MediaID: "v1", Raw: "Hello Seyal"}
	for _, c := range []struct {
		name string
		msg  channel.Message
		want bool
	}{
		{"typed", channel.Message{Text: "Hello"}, true},
		{"spoken: its own recording doesn't count", channel.Message{Text: "Hello Seyal", Media: []channel.Media{voice}, Transcript: spoken}, true},
		{"spoken, with a photo", channel.Message{Text: "Hello Seyal", Media: []channel.Media{voice, photo}, Transcript: spoken}, false},
		{"spoken, words deleted", channel.Message{Text: " ", Media: []channel.Media{voice}, Transcript: spoken}, false},
		{"a voice note without words", channel.Message{Media: []channel.Media{voice}}, false},
		{"text with a photo", channel.Message{Text: "Hello", Media: []channel.Media{photo}}, false},
	} {
		if got := wordsOnly(c.msg); got != c.want {
			t.Errorf("%s: wordsOnly = %v, want %v", c.name, got, c.want)
		}
	}
}
