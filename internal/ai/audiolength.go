// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/bits"
)

// How long a recording is, read from the file itself (D66). Speech services charge by the length of
// the audio, not by the size of the file, and a file of a few MB at a very low quality can hold hours.
// So Seyal measures every recording before sending it, and sends only recordings it can measure, up
// to MaxSeconds.
//
// The length is counted from the sound in the file, not from the length the file says it has (a
// file made by hand could say anything there):
//   - Opus in WebM or Ogg (Chrome, Edge, Firefox): the first byte of each Opus packet says how much
//     sound it holds;
//   - AAC in MP4 (Safari): every AAC frame holds 1024 samples, and the file says how many frames
//     there are and how many samples make a second.
//
// Seyal reads the same settings a player decodes with (the track's codec in WebM; the AAC settings
// in MP4), so a file can't name one codec or speed for the measuring and play with another.
// Anything else (another sound format, or a file built in a way the composer doesn't make) is not
// measured, so it isn't sent. As a second check, Transcribe logs a warning when the service says it
// heard much longer than Seyal measured.

// MaxSeconds is the longest recording sent to a speech service: 6 minutes. The composer stops
// recording after 5.
const MaxSeconds = 6 * 60

// ErrUnmeasured means Seyal can't read how long the recording is, so it doesn't send it.
var ErrUnmeasured = errors.New("the recording's length can't be read")

// audioSeconds returns the length of the recording in seconds, or ErrUnmeasured.
func audioSeconds(b []byte) (float64, error) {
	var secs float64
	var ok bool
	switch {
	case len(b) >= 4 && binary.BigEndian.Uint32(b) == mkvEBML:
		secs, ok = webmSeconds(b)
	case bytes.HasPrefix(b, []byte("OggS")):
		secs, ok = oggSeconds(b)
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		secs, ok = mp4Seconds(b)
	}
	if !ok {
		return 0, ErrUnmeasured
	}
	return secs, nil
}

// opusSeconds is the sound in one Opus packet (RFC 6716, section 3.1): the top 5 bits of the first
// byte give the length of a frame, the low 2 bits how many frames there are.
func opusSeconds(p []byte) (float64, bool) {
	if len(p) == 0 {
		return 0, true // an empty packet holds no sound
	}
	var ms float64
	switch config := p[0] >> 3; {
	case config < 12: // SILK
		ms = [4]float64{10, 20, 40, 60}[config%4]
	case config < 16: // hybrid
		ms = [2]float64{10, 20}[config%2]
	default: // CELT
		ms = [4]float64{2.5, 5, 10, 20}[config%4]
	}
	frames := 1
	switch p[0] & 3 {
	case 1, 2:
		frames = 2
	case 3: // the second byte says how many
		if len(p) < 2 {
			return 0, false
		}
		frames = int(p[1] & 0x3f)
	}
	return float64(frames) * ms / 1000, true
}

// WebM (Matroska) element ids used here.
const (
	mkvEBML         = 0x1A45DFA3
	mkvSegment      = 0x18538067
	mkvCluster      = 0x1F43B675
	mkvTracks       = 0x1654AE6B
	mkvTrackEntry   = 0xAE
	mkvTrackNumber  = 0xD7
	mkvCodecID      = 0x86
	mkvBlockGroup   = 0xA0
	mkvBlock        = 0xA1
	mkvSimpleBlock  = 0xA3
	mkvTimestamp    = 0xE7
	mkvSilentTracks = 0x5854
	mkvPosition     = 0xA7
	mkvPrevSize     = 0xAB
	mkvEncrypted    = 0xAF
	mkvVoid         = 0xEC
	mkvCRC32        = 0xBF
)

// mkvOpen is an element Seyal is looking inside: its id, and where it ends (-1: "unknown size", it
// runs until an element that can't be inside it).
type mkvOpen struct {
	id  uint32
	end int
}

// mkvInside reports whether Seyal looks inside element id when it is found inside parent (0 for the
// top of the file). Only these hold what is measured; any other element is stepped over, as players
// step over elements found in the wrong place.
func mkvInside(parent, id uint32) bool {
	switch parent {
	case 0:
		return id == mkvSegment
	case mkvSegment:
		return id == mkvTracks || id == mkvCluster
	case mkvTracks:
		return id == mkvTrackEntry
	case mkvCluster:
		return id == mkvBlockGroup
	}
	return false
}

// mkvInCluster reports whether id can be inside a cluster. Browsers write clusters with an unknown
// size: such a cluster ends at the first element that can't be inside it (usually the next cluster).
func mkvInCluster(id uint32) bool {
	switch id {
	case mkvTimestamp, mkvSilentTracks, mkvPosition, mkvPrevSize, mkvSimpleBlock, mkvBlockGroup, mkvEncrypted,
		mkvVoid, mkvCRC32:
		return true
	}
	return false
}

// webmSeconds adds up the Opus packets in a WebM file with one Opus track. It walks the elements in
// order, keeping track of which element each one is inside: it looks inside the ones that hold the
// track and the sound (segment > tracks > track entry, segment > cluster > block group) and steps
// over every other one. A player decodes the sound with the codec the track entry names, so the
// track must say exactly "A_OPUS" there, once; and only blocks of that track, inside clusters, count.
func webmSeconds(b []byte) (float64, bool) {
	var secs float64
	var open []mkvOpen
	tracks, codecs := 0, 0
	var track uint64 // the track's number, from its track entry
	for i := 0; i < len(b); {
		// Close the elements that end here.
		for len(open) > 0 && open[len(open)-1].end >= 0 && i >= open[len(open)-1].end {
			open = open[:len(open)-1]
		}
		limit := len(b) // where the innermost element with a known size ends
		for k := len(open) - 1; k >= 0; k-- {
			if open[k].end >= 0 {
				limit = open[k].end
				break
			}
		}
		id, n := ebmlID(b[i:limit])
		if n == 0 {
			return 0, false
		}
		// An element that can't be in a cluster of unknown size ends that cluster.
		for len(open) > 0 && open[len(open)-1].end < 0 && open[len(open)-1].id == mkvCluster && !mkvInCluster(id) {
			open = open[:len(open)-1]
		}
		var parent uint32
		if len(open) > 0 {
			parent = open[len(open)-1].id
		}
		i += n
		size, n, unknown := ebmlSize(b[i:limit])
		if n == 0 {
			return 0, false
		}
		i += n
		if !unknown && size > uint64(limit-i) {
			return 0, false
		}
		if mkvInside(parent, id) {
			end := -1
			switch {
			case !unknown:
				end = i + int(size)
			case id != mkvSegment && id != mkvCluster: // browsers leave only these two without a size
				return 0, false
			}
			if id == mkvTrackEntry {
				tracks++
			}
			open = append(open, mkvOpen{id: id, end: end})
			continue // look inside: its children follow
		}
		if unknown {
			return 0, false
		}
		if id == mkvSimpleBlock && parent != mkvCluster || id == mkvBlockGroup || id == mkvBlock && parent != mkvBlockGroup {
			return 0, false // sound out of place (a block group in its place was looked inside above)
		}
		data := b[i : i+int(size)]
		i += int(size)
		switch {
		case parent == mkvTrackEntry && id == mkvCodecID:
			if string(data) != "A_OPUS" {
				return 0, false // another codec: its packets hold sound in other ways
			}
			codecs++
		case parent == mkvTrackEntry && id == mkvTrackNumber:
			if len(data) == 0 || len(data) > 8 {
				return 0, false
			}
			for _, c := range data {
				track = track<<8 | uint64(c)
			}
		case parent == mkvCluster && id == mkvSimpleBlock, parent == mkvBlockGroup && id == mkvBlock:
			s, num, ok := blockSeconds(data)
			if !ok || tracks != 1 || codecs != 1 || track == 0 || num != track {
				return 0, false // sound before the track, or for another track
			}
			secs += s
		}
	}
	if tracks != 1 || codecs != 1 {
		return 0, false
	}
	return secs, true
}

// blockSeconds reads one WebM block: the track number, a 2-byte time, a flags byte, then one Opus
// packet. Blocks that pack several packets together ("lacing") aren't made by browsers, so they
// aren't measured.
func blockSeconds(b []byte) (secs float64, track uint64, ok bool) {
	track, n, _ := ebmlSize(b) // the track number
	if n == 0 || len(b) < n+3 || b[n+2]&0x06 != 0 {
		return 0, 0, false
	}
	secs, ok = opusSeconds(b[n+3:])
	return secs, track, ok
}

// ebmlID reads an element id (1 to 4 bytes; the first byte says how many). n is 0 if it can't.
func ebmlID(b []byte) (id uint32, n int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if n > 4 || len(b) < n {
		return 0, 0
	}
	for _, c := range b[:n] {
		id = id<<8 | uint32(c)
	}
	return id, n
}

// ebmlSize reads an element size (1 to 8 bytes; the first byte says how many). All bits set means
// "unknown size": browsers write the segment and clusters that way while still recording.
func ebmlSize(b []byte) (size uint64, n int, unknown bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if len(b) < n {
		return 0, 0, false
	}
	size = uint64(b[0]) & (0xff >> n)
	for _, c := range b[1:n] {
		size = size<<8 | uint64(c)
	}
	return size, n, size == 1<<(7*n)-1
}

// oggSeconds adds up the Opus packets in an Ogg file with one stream. Each page has a small header
// and a table of piece lengths; a piece shorter than 255 bytes ends a packet. The first two packets
// are the Opus headers; every later one is sound.
func oggSeconds(b []byte) (float64, bool) {
	var secs float64
	var packet []byte
	var serial uint32
	packets := 0
	for i := 0; i < len(b); {
		if len(b)-i < 27 || string(b[i:i+4]) != "OggS" || b[i+4] != 0 {
			return 0, false
		}
		if s := binary.LittleEndian.Uint32(b[i+14:]); i == 0 {
			serial = s
		} else if s != serial {
			return 0, false // a second stream
		}
		pieces := int(b[i+26])
		if len(b)-i < 27+pieces {
			return 0, false
		}
		table := b[i+27 : i+27+pieces]
		i += 27 + pieces
		for _, l := range table {
			if len(b)-i < int(l) {
				return 0, false
			}
			packet = append(packet, b[i:i+int(l)]...)
			i += int(l)
			if l == 255 {
				continue // the packet goes on
			}
			switch packets {
			case 0:
				if !bytes.HasPrefix(packet, []byte("OpusHead")) {
					return 0, false
				}
			case 1:
				if !bytes.HasPrefix(packet, []byte("OpusTags")) {
					return 0, false
				}
			default:
				s, ok := opusSeconds(packet)
				if !ok {
					return 0, false
				}
				secs += s
			}
			packets++
			packet = packet[:0]
		}
	}
	if packets < 2 {
		return 0, false
	}
	return secs, true
}

// mp4Seconds counts the AAC frames in an MP4 file with one track. Safari writes the file in pieces
// ("fragments"), each saying how many frames it holds (trun); a file written in one go says it once
// (stsz). The samples per second come from the AAC settings in the track's sample entry (esds), which
// is what players decode with.
func mp4Seconds(b []byte) (float64, bool) {
	var m mp4Count
	if !m.walk(b, 0) || m.tracks != 1 || m.entries != 1 || m.rate <= 0 {
		return 0, false
	}
	return float64(m.frames) * 1024 / float64(m.rate), true
}

type mp4Count struct {
	tracks, entries, rate int
	frames                uint64
}

// mp4Depth is how deep boxes are looked inside. Real files go 5 deep (moov > trak > mdia > minf >
// stbl); a file that goes deeper was made by hand, and walking it would use a lot of memory.
const mp4Depth = 8

// mp4Box reads the first box in b (a 4-byte size, a 4-byte type, then its content).
func mp4Box(b []byte) (typ string, body, rest []byte, ok bool) {
	if len(b) < 8 {
		return "", nil, nil, false
	}
	size, head := uint64(binary.BigEndian.Uint32(b)), uint64(8)
	switch size {
	case 0: // to the end of the file
		size = uint64(len(b))
	case 1: // a 64-bit size follows the type
		if len(b) < 16 {
			return "", nil, nil, false
		}
		size, head = binary.BigEndian.Uint64(b[8:]), 16
	}
	if size < head || size > uint64(len(b)) {
		return "", nil, nil, false
	}
	return string(b[4:8]), b[head:size], b[size:], true
}

// walk reads the boxes in b, looking inside the ones that lead to the track's description and the
// fragments. depth is how deep b is.
func (m *mp4Count) walk(b []byte, depth int) bool {
	if depth > mp4Depth {
		return false
	}
	for len(b) > 0 {
		typ, body, rest, ok := mp4Box(b)
		if !ok {
			return false
		}
		b = rest
		switch typ {
		case "moov", "trak", "mdia", "minf", "stbl", "moof", "traf":
			if typ == "trak" {
				m.tracks++
			}
			if !m.walk(body, depth+1) {
				return false
			}
		case "stsd":
			m.entries++
			rate, ok := mp4aRate(body)
			if !ok {
				return false
			}
			m.rate = rate
		case "stsz", "stz2": // version and flags (4), 4 more bytes, then how many frames
			if len(body) < 12 {
				return false
			}
			m.frames += uint64(binary.BigEndian.Uint32(body[8:]))
		case "trun": // version and flags (4), then how many frames
			if len(body) < 8 {
				return false
			}
			m.frames += uint64(binary.BigEndian.Uint32(body[4:]))
		}
	}
	return true
}

// mp4aRate reads the track's description (stsd): exactly one sample entry, of type mp4a, holding AAC
// (its esds box). It returns the samples per second of the AAC core: each frame holds 1024 of them.
//
// The entry: version and flags (4), how many entries (4), then the entry: size and type (8), 6+2
// bytes we don't need, a version (2), 6 more, channels, sample size, 2+2 more, the samples per second
// as a 16.16 number (4), then (version 1 only) 16 more bytes, then boxes, one of them esds. The
// samples per second written in the entry must agree with the AAC settings.
func mp4aRate(body []byte) (int, bool) {
	if len(body) < 8 || binary.BigEndian.Uint32(body[4:]) != 1 {
		return 0, false
	}
	typ, entry, rest, ok := mp4Box(body[8:])
	if !ok || typ != "mp4a" || len(rest) != 0 || len(entry) < 28 {
		return 0, false
	}
	said := int(binary.BigEndian.Uint16(entry[24:]))
	boxes := entry[28:]
	switch binary.BigEndian.Uint16(entry[8:]) {
	case 0:
	case 1: // QuickTime's version 1: 16 more bytes
		if len(boxes) < 16 {
			return 0, false
		}
		boxes = boxes[16:]
	default:
		return 0, false
	}
	var config []byte
	for len(boxes) > 0 {
		typ, body, rest, ok := mp4Box(boxes)
		if !ok {
			return 0, false
		}
		boxes = rest
		if typ == "esds" {
			if config != nil {
				return 0, false // two sets of settings
			}
			if config, ok = esdsConfig(body); !ok {
				return 0, false
			}
		}
	}
	if config == nil {
		return 0, false
	}
	core, out, ok := aacRates(config)
	if !ok || (said != core && said != out) {
		return 0, false
	}
	return core, true
}

// esdsConfig reads an esds box (ISO/IEC 14496-1, 7.2.6): version and flags (4), then an
// ES_Descriptor holding a DecoderConfigDescriptor, which says the codec (0x40: MPEG-4 audio) and holds
// the AAC settings (AudioSpecificConfig).
func esdsConfig(b []byte) ([]byte, bool) {
	if len(b) < 4 {
		return nil, false
	}
	tag, es, _, ok := mp4Descriptor(b[4:])
	if !ok || tag != 0x03 || len(es) < 3 {
		return nil, false
	}
	flags := es[2]
	es = es[3:]          // the stream's id (2) and the flags (1)
	if flags&0x80 != 0 { // the stream it depends on
		if len(es) < 2 {
			return nil, false
		}
		es = es[2:]
	}
	if flags&0x40 != 0 { // a web address
		if len(es) < 1 || len(es) < 1+int(es[0]) {
			return nil, false
		}
		es = es[1+int(es[0]):]
	}
	if flags&0x20 != 0 { // a clock stream
		if len(es) < 2 {
			return nil, false
		}
		es = es[2:]
	}
	tag, dc, _, ok := mp4Descriptor(es)
	// The codec (1), the stream type (1), buffer size (3), bit rates (4+4), then the settings.
	if !ok || tag != 0x04 || len(dc) < 13 || dc[0] != 0x40 {
		return nil, false
	}
	tag, config, _, ok := mp4Descriptor(dc[13:])
	if !ok || tag != 0x05 {
		return nil, false
	}
	return config, true
}

// mp4Descriptor reads one descriptor: a tag (1), its size (1 to 4 bytes, 7 bits each; the top bit
// says another byte follows), then its content.
func mp4Descriptor(b []byte) (tag byte, body, rest []byte, ok bool) {
	if len(b) < 2 {
		return 0, nil, nil, false
	}
	size, i := 0, 1
	for ; ; i++ {
		if i > 4 || i >= len(b) {
			return 0, nil, nil, false
		}
		size = size<<7 | int(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			break
		}
	}
	i++
	if size > len(b)-i {
		return 0, nil, nil, false
	}
	return b[0], b[i : i+size], b[i+size:], true
}

// aacSampleRates are the samples per second that AAC settings name by number.
var aacSampleRates = [13]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// aacRates reads AAC settings (AudioSpecificConfig, ISO/IEC 14496-3, 1.6.2.1): the samples per second
// of the AAC core, and of the sound that comes out (twice as many with SBR, which also doubles the
// samples in each frame, so the length is the same). Only the AAC types whose frames hold 1024
// samples (or fewer) are measured: Main, LC (what Safari records), SSR and LTP, also with SBR or PS.
func aacRates(config []byte) (core, out int, ok bool) {
	r := bitReader{b: config}
	objectType := func() uint32 {
		if t := r.read(5); t != 31 {
			return t
		}
		return 32 + r.read(6)
	}
	rate := func() int {
		i := r.read(4)
		if i == 15 { // written out
			return int(r.read(24))
		}
		if int(i) >= len(aacSampleRates) {
			return 0
		}
		return aacSampleRates[i]
	}
	t := objectType()
	core = rate()
	r.read(4) // channels
	out = core
	if t == 5 || t == 29 { // SBR, or PS: the rate that comes out, then the core's own type
		out = rate()
		t = objectType()
	}
	if r.short || t < 1 || t > 4 || core <= 0 || out <= 0 {
		return 0, 0, false
	}
	return core, out, true
}

// bitReader reads numbers of a few bits from b, the first bit first. short is set when b ran out.
type bitReader struct {
	b     []byte
	pos   int // in bits
	short bool
}

func (r *bitReader) read(n int) uint32 {
	var v uint32
	for range n {
		if r.pos >= len(r.b)*8 {
			r.short = true
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

// FakeRecording is a WebM file holding seconds of Opus sound (silence, in 20 ms packets), for tests
// and demos: Seyal measures it like a recording from the composer.
func FakeRecording(seconds int) []byte {
	track := mkvElement([]byte{0xD7}, []byte{1})                         // track number 1
	track = append(track, mkvElement([]byte{0x86}, []byte("A_OPUS"))...) // codec
	return fakeWebM(seconds, track)
}

// mkvElement is one WebM element: its id, its size, then data. The size is written in 8 bytes (0x01
// then 7 bytes), which keeps this simple for any length.
func mkvElement(id []byte, data []byte) []byte {
	size := binary.BigEndian.AppendUint64(nil, uint64(len(data)))
	size[0] = 0x01
	return append(append(append([]byte{}, id...), size...), data...)
}

// fakeWebM is FakeRecording with track as the content of its track entry.
func fakeWebM(seconds int, track []byte) []byte {
	el := mkvElement
	unknown := []byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	var out []byte
	out = append(out, el([]byte{0x1A, 0x45, 0xDF, 0xA3}, el([]byte{0x42, 0x82}, []byte("webm")))...)
	out = append(out, 0x18, 0x53, 0x80, 0x67)
	out = append(out, unknown...)
	out = append(out, el([]byte{0x16, 0x54, 0xAE, 0x6B}, el([]byte{0xAE}, track))...)
	packets := seconds * 50              // 20 ms each
	for p := 0; p < packets; p += 1000 { // a cluster every 20 seconds
		cluster := el([]byte{0xE7}, binary.BigEndian.AppendUint32(nil, uint32(p*20)))
		for q := p; q < min(p+1000, packets); q++ {
			t := uint16((q - p) * 20)
			// track 1, the time within the cluster, a keyframe; then a 20 ms CELT packet of silence
			cluster = append(cluster, el([]byte{0xA3}, []byte{0x81, byte(t >> 8), byte(t), 0x80, 0xF8, 0xFF, 0xFE})...)
		}
		out = append(out, 0x1F, 0x43, 0xB6, 0x75)
		out = append(out, unknown...)
		out = append(out, cluster...)
	}
	return out
}
