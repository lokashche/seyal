// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"
)

func TestOpusPacketLength(t *testing.T) {
	for _, c := range []struct {
		packet []byte
		ms     float64
	}{
		{[]byte{0xF8, 0xFF, 0xFE}, 20}, // CELT 20 ms, one frame (what browsers send)
		{[]byte{31<<3 | 1}, 40},        // two frames of 20 ms
		{[]byte{31<<3 | 3, 0x03}, 60},  // the second byte says 3 frames
		{[]byte{3<<3 | 0}, 60},         // SILK 60 ms
		{[]byte{13<<3 | 0}, 20},        // hybrid 20 ms
		{[]byte{16<<3 | 3, 0x30}, 120}, // 48 CELT frames of 2.5 ms
		{nil, 0},                       // an empty packet holds no sound
	} {
		got, ok := opusSeconds(c.packet)
		if !ok || math.Abs(got*1000-c.ms) > 1e-9 {
			t.Errorf("opusSeconds(% x) = %v ms, %v; want %v ms", c.packet, got*1000, ok, c.ms)
		}
	}
	if _, ok := opusSeconds([]byte{31<<3 | 3}); ok {
		t.Error("a packet that says 'count in the next byte' without that byte was measured")
	}
}

func TestWebMLength(t *testing.T) {
	for _, secs := range []int{1, 25, 7 * 60} {
		got, err := audioSeconds(FakeRecording(secs))
		if err != nil || math.Abs(got-float64(secs)) > 1e-6 {
			t.Errorf("FakeRecording(%d) measured %v, %v", secs, got, err)
		}
	}
	good := FakeRecording(2)
	if _, err := audioSeconds(good[:len(good)-3]); !errors.Is(err, ErrUnmeasured) {
		t.Errorf("a cut-off file = %v, want ErrUnmeasured", err)
	}
	for name, change := range map[string][2]string{
		"another codec":       {"A_OPUS", "A_VORB"},
		"laced blocks":        {"\x81\x00\x00\x80\xF8", "\x81\x00\x00\x82\xF8"},
		"an unknown size tag": {"\xD7\x01\x00\x00\x00\x00\x00\x00\x01", "\xD7\x01\xff\xff\xff\xff\xff\xff\xff"},
	} {
		bad := bytes.Replace(good, []byte(change[0]), []byte(change[1]), 1)
		if bytes.Equal(bad, good) {
			t.Fatalf("%s: nothing changed", name)
		}
		if _, err := audioSeconds(bad); !errors.Is(err, ErrUnmeasured) {
			t.Errorf("%s = %v, want ErrUnmeasured", name, err)
		}
	}
	// Tracks > TrackEntry > CodecID "A_OPUS": a second track.
	secondTrack := append([]byte{0x16, 0x54, 0xAE, 0x6B, 0x8A, 0xAE, 0x88, 0x86, 0x86}, "A_OPUS"...)
	if _, err := audioSeconds(append(append([]byte{}, good...), secondTrack...)); !errors.Is(err, ErrUnmeasured) {
		t.Errorf("two tracks = %v, want ErrUnmeasured", err)
	}
	// Elements Seyal doesn't need (here an empty Void) are stepped over.
	if got, err := audioSeconds(append(append([]byte{}, good...), 0xEC, 0x80)); err != nil || math.Abs(got-2) > 1e-9 {
		t.Errorf("with a Void element = %v, %v", got, err)
	}
}

// TestWebMCodecIsTheTracks: only the codec in the track entry counts, as players read it. A file
// whose track is another codec can't pass by carrying a stray "A_OPUS" somewhere else, and only
// the track's own blocks, inside clusters, are its sound.
func TestWebMCodecIsTheTracks(t *testing.T) {
	number := mkvElement([]byte{0xD7}, []byte{1})
	codec := func(name string) []byte { return mkvElement([]byte{0x86}, []byte(name)) }
	stray := codec("A_OPUS")
	vorbis := fakeWebM(2, slices.Concat(number, codec("A_VORBIS")))
	segment := bytes.Index(vorbis, []byte{0x18, 0x53, 0x80, 0x67}) + 12 // just inside the segment
	cluster := bytes.Index(vorbis, []byte{0x1F, 0x43, 0xB6, 0x75}) + 12 // just inside the first cluster
	good := FakeRecording(2)
	block := bytes.Index(good, []byte{0xA3, 0x01}) // the first block
	for name, file := range map[string][]byte{
		"a Vorbis track":                        vorbis,
		"a Vorbis track, A_OPUS in the segment": slices.Concat(vorbis[:segment], stray, vorbis[segment:]),
		"a Vorbis track, A_OPUS in a cluster":   slices.Concat(vorbis[:cluster], stray, vorbis[cluster:]),
		"a track that names two codecs":         fakeWebM(2, slices.Concat(number, stray, codec("A_VORBIS"))),
		"a track that names Opus twice":         fakeWebM(2, slices.Concat(number, stray, stray)),
		"a track with no number":                fakeWebM(2, stray),
		"blocks of another track":               fakeWebM(2, slices.Concat(mkvElement([]byte{0xD7}, []byte{2}), stray)),
		"a block outside any cluster":           slices.Concat(good[:segment], good[block:block+16], good[segment:]),
	} {
		if _, err := audioSeconds(file); !errors.Is(err, ErrUnmeasured) {
			t.Errorf("%s = %v, want ErrUnmeasured", name, err)
		}
	}
	// A CodecID that isn't in a track entry is stepped over, as players do.
	withStray := slices.Concat(good[:segment], stray, good[segment:])
	if got, err := audioSeconds(withStray); err != nil || math.Abs(got-2) > 1e-9 {
		t.Errorf("an Opus file with a stray CodecID = %v, %v; want 2", got, err)
	}
}

// oggPage is one Ogg page of stream 7 holding the pieces (each at most 255 bytes) in data.
func oggPage(table []byte, data []byte) []byte {
	p := append([]byte("OggS"), 0, 0)
	p = append(p, make([]byte, 8)...)          // granule position: not read
	p = binary.LittleEndian.AppendUint32(p, 7) // stream
	p = append(p, make([]byte, 8)...)          // page number and checksum: not read
	p = append(append(append(p, byte(len(table))), table...), data...)
	return p
}

func TestOggLength(t *testing.T) {
	head := append([]byte("OpusHead"), make([]byte, 11)...)
	tags := append([]byte("OpusTags"), make([]byte, 8)...)
	file := oggPage([]byte{byte(len(head))}, head)
	file = append(file, oggPage([]byte{byte(len(tags))}, tags)...)
	// 50 packets of 20 ms on one page.
	var table, data []byte
	for range 50 {
		table, data = append(table, 3), append(data, 0xF8, 0xFF, 0xFE)
	}
	file = append(file, oggPage(table, data)...)
	// One packet of 3 × 20 ms, 300 bytes long, split over two pages (a piece of 255 goes on).
	long := append([]byte{31<<3 | 3, 3}, make([]byte, 298)...)
	file = append(file, oggPage([]byte{255}, long[:255])...)
	file = append(file, oggPage([]byte{45}, long[255:])...)
	got, err := audioSeconds(file)
	if err != nil || math.Abs(got-1.06) > 1e-9 {
		t.Errorf("ogg measured %v, %v; want 1.06", got, err)
	}
	notOpus := bytes.Replace(file, []byte("OpusHead"), []byte("VorbisHd"), 1)
	if _, err := audioSeconds(notOpus); !errors.Is(err, ErrUnmeasured) {
		t.Errorf("ogg without Opus = %v", err)
	}
	other := oggPage([]byte{3}, []byte{0xF8, 0xFF, 0xFE})
	binary.LittleEndian.PutUint32(other[14:], 8)
	if _, err := audioSeconds(append(append([]byte{}, file...), other...)); !errors.Is(err, ErrUnmeasured) {
		t.Errorf("ogg with a second stream = %v", err)
	}
}

func box(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(b, typ...), body...)
}

// bitsOf writes numbers of a few bits each, the first bit first: pairs of (value, bits).
func bitsOf(pairs ...uint32) []byte {
	var out []byte
	n := 0
	for i := 0; i < len(pairs); i += 2 {
		for b := int(pairs[i+1]) - 1; b >= 0; b-- {
			if n%8 == 0 {
				out = append(out, 0)
			}
			out[len(out)-1] |= byte(pairs[i]>>b&1) << (7 - n%8)
			n++
		}
	}
	return out
}

// esds is an esds box's content: the codec (0x40: MPEG-4 audio) and the AAC settings.
func esds(codec byte, config []byte) []byte {
	desc := func(tag byte, body ...[]byte) []byte {
		b := bytes.Join(body, nil)
		return append([]byte{tag, byte(len(b))}, b...)
	}
	dc := desc(0x04, []byte{codec, 0x15}, make([]byte, 11), desc(0x05, config))
	return append(make([]byte, 4), desc(0x03, []byte{0, 1, 0}, dc, desc(0x06, []byte{2}))...)
}

// aacLC is the AAC settings of AAC-LC (what Safari records) at the rate with that number in the
// table, one channel.
func aacLC(index uint32) []byte { return bitsOf(2, 5, index, 4, 1, 4, 0, 3) }

// mp4Audio is an MP4 file with one AAC-LC track at rate samples per second, and frames frames: in
// fragments like Safari's, or said once in stsz.
func mp4Audio(rate int, codec string, fragments []int, stsz int) []byte {
	index := slices.Index(aacSampleRates[:], rate)
	return mp4File(rate, codec, box("esds", esds(0x40, aacLC(uint32(index)))), fragments, stsz)
}

// mp4File is mp4Audio with the samples per second the sample entry says (said) and the boxes in it
// (extra) chosen.
func mp4File(said int, codec string, extra []byte, fragments []int, stsz int) []byte {
	entry := make([]byte, 28)
	binary.BigEndian.PutUint16(entry[16:], 1) // channels
	binary.BigEndian.PutUint32(entry[24:], uint32(said)<<16)
	stsd := box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, box(codec, entry, extra))
	stbl := box("stbl", stsd, box("stsz", make([]byte, 8), binary.BigEndian.AppendUint32(nil, uint32(stsz))))
	moov := box("moov", box("mvhd", make([]byte, 100)),
		box("trak", box("tkhd", make([]byte, 84)), box("mdia", box("mdhd", make([]byte, 24)), box("minf", stbl))),
		box("mvex", box("trex", make([]byte, 24))))
	file := append(box("ftyp", []byte("iso5"), make([]byte, 4)), moov...)
	for _, n := range fragments {
		trun := box("trun", make([]byte, 4), binary.BigEndian.AppendUint32(nil, uint32(n)))
		file = append(file, box("moof", box("mfhd", make([]byte, 8)), box("traf", box("tfhd", make([]byte, 8)), trun))...)
		file = append(file, box("mdat", make([]byte, n*10))...)
	}
	return file
}

func TestMP4Length(t *testing.T) {
	got, err := audioSeconds(mp4Audio(48000, "mp4a", []int{100, 100, 50}, 0))
	if want := 250 * 1024 / 48000.0; err != nil || math.Abs(got-want) > 1e-9 {
		t.Errorf("fragmented mp4 measured %v, %v; want %v", got, err, want)
	}
	got, err = audioSeconds(mp4Audio(44100, "mp4a", nil, 441))
	if want := 441 * 1024 / 44100.0; err != nil || math.Abs(got-want) > 1e-9 {
		t.Errorf("mp4 in one piece measured %v, %v; want %v", got, err, want)
	}
	if _, err := audioSeconds(mp4Audio(48000, "Opus", []int{100}, 0)); !errors.Is(err, ErrUnmeasured) {
		t.Errorf("mp4 that isn't AAC = %v", err)
	}
	cut := mp4Audio(48000, "mp4a", []int{100}, 0)
	if _, err := audioSeconds(cut[:len(cut)-5]); !errors.Is(err, ErrUnmeasured) {
		t.Errorf("a cut-off mp4 = %v", err)
	}
}

// TestMP4RateIsTheDecoders: the samples per second come from the AAC settings (esds), which players
// decode with; the number in the sample entry must agree. A file can't say a fast rate there and be
// played at a slow one (here 65535 said, 7350 decoded: about 9 times longer than measured).
func TestMP4RateIsTheDecoders(t *testing.T) {
	aac48 := box("esds", esds(0x40, aacLC(3)))
	sbr := box("esds", esds(0x40, bitsOf(5, 5, 6, 4, 1, 4, 3, 4, 2, 5, 0, 3))) // SBR: core 24000, out 48000
	written := box("esds", esds(0x40, bitsOf(2, 5, 15, 4, 16000, 24, 1, 4, 0, 3)))
	for name, c := range map[string]struct {
		file []byte
		want float64
	}{
		"SBR: the core's rate counts":       {mp4File(48000, "mp4a", sbr, []int{100}, 0), 100 * 1024 / 24000.0},
		"SBR, the entry saying the core's":  {mp4File(24000, "mp4a", sbr, []int{100}, 0), 100 * 1024 / 24000.0},
		"a rate written out, not by number": {mp4File(16000, "mp4a", written, []int{100}, 0), 100 * 1024 / 16000.0},
	} {
		got, err := audioSeconds(c.file)
		if err != nil || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: measured %v, %v; want %v", name, got, err, c.want)
		}
	}
	for name, file := range map[string][]byte{
		"the entry says 65535, the AAC settings 7350": mp4File(65535, "mp4a", box("esds", esds(0x40, aacLC(12))), []int{100}, 0),
		"the entry says 48000, the AAC settings 8000": mp4File(48000, "mp4a", box("esds", esds(0x40, aacLC(11))), []int{100}, 0),
		"no AAC settings":                  mp4File(48000, "mp4a", nil, []int{100}, 0),
		"two sets of AAC settings":         mp4File(48000, "mp4a", slices.Concat(aac48, aac48), []int{100}, 0),
		"MP3 inside (codec 0x6B)":          mp4File(48000, "mp4a", box("esds", esds(0x6B, aacLC(3))), []int{100}, 0),
		"MPEG-2 AAC (codec 0x67)":          mp4File(48000, "mp4a", box("esds", esds(0x67, aacLC(3))), []int{100}, 0),
		"USAC, whose frames can be longer": mp4File(48000, "mp4a", box("esds", esds(0x40, bitsOf(31, 5, 10, 6, 3, 4, 1, 4))), []int{100}, 0),
		"a rate number not in the table":   mp4File(48000, "mp4a", box("esds", esds(0x40, aacLC(13))), []int{100}, 0),
		"cut-off AAC settings":             mp4File(48000, "mp4a", box("esds", esds(0x40, []byte{0x11})), []int{100}, 0),
	} {
		if _, err := audioSeconds(file); !errors.Is(err, ErrUnmeasured) {
			t.Errorf("%s = %v, want ErrUnmeasured", name, err)
		}
	}
}

// TestMP4DeepBoxes: boxes nested far deeper than real files are refused quickly, without walking
// every level (a 10 MB file could nest over a million).
func TestMP4DeepBoxes(t *testing.T) {
	file := box("ftyp", []byte("iso5"), make([]byte, 4))
	for range 100_000 {
		file = append(file, 0, 0, 0, 0, 'm', 'o', 'o', 'v') // size 0: to the end of the file
	}
	if _, err := audioSeconds(file); !errors.Is(err, ErrUnmeasured) {
		t.Errorf("deep boxes = %v, want ErrUnmeasured", err)
	}
	m := mp4Count{}
	if m.walk(file[16:], mp4Depth-2) {
		t.Error("walk went past the depth limit")
	}
}

func TestUnknownAudioIsNotMeasured(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("pretend opus audio"), []byte("RIFF\x00\x00\x00\x00WAVE"), []byte("ID3\x04")} {
		if _, err := audioSeconds(b); !errors.Is(err, ErrUnmeasured) {
			t.Errorf("audioSeconds(%q) = %v, want ErrUnmeasured", b, err)
		}
	}
}

// TestSentSeconds: the length counted against a business's minutes a day is the length of what
// Transcribe sends; a recording it doesn't send counts nothing.
func TestSentSeconds(t *testing.T) {
	if got := SentSeconds(FakeRecording(12)); math.Abs(got-12) > 0.01 {
		t.Errorf("SentSeconds(12 s) = %v", got)
	}
	tooLarge := append(FakeRecording(1), make([]byte, MaxAudio)...)
	for name, b := range map[string][]byte{"empty": nil, "not measurable": []byte("pretend opus audio"),
		"too long": FakeRecording(MaxSeconds + 1), "too large": tooLarge} {
		if got := SentSeconds(b); got != 0 {
			t.Errorf("SentSeconds(%s) = %v, want 0", name, got)
		}
	}
}
