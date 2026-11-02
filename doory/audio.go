// Audio uploads: recognise the container from its bytes and check that it is
// coherent. No decoder is involved: MP3, Ogg (Opus/Vorbis/FLAC), WAVE,
// M4A (audio-only ISO base media) and WebM (opus/vorbis, no video) are read
// from their headers, exactly like assets/audio.py.
package doory

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

const (
	audioMaxID3Bytes     = 4 * 1024 * 1024
	audioMaxFrameWalk    = 400_000
	audioMaxDurationMS   = 24 * 60 * 60 * 1000
	audioMaxChunks       = 256
	audioMaxBoxDepth     = 8
	audioMaxBoxes        = 50_000
	audioMaxEBMLElements = 4096
	audioEBMLScanBytes   = 2 * 1024 * 1024
)

const (
	audioMP3MIME  = "audio/mpeg"
	audioOGGMIME  = "audio/ogg"
	audioWAVMIME  = "audio/wav"
	audioM4AMIME  = "audio/mp4"
	audioWebMMIME = "audio/webm"
)

var audioExtensions = map[string]string{audioMP3MIME: "mp3", audioOGGMIME: "ogg",
	audioWAVMIME: "wav", audioM4AMIME: "m4a", audioWebMMIME: "weba"}

var audioBrands = map[string]bool{"M4A ": true, "M4B ": true, "M4P ": true, "F4A ": true,
	"mp41": true, "mp42": true, "isom": true, "iso2": true, "iso4": true, "iso5": true,
	"iso6": true, "dash": true, "3gp4": true, "3gp5": true, "3gp6": true, "3g2a": true, "qt  ": true}

type audioError struct{ message string }

func (e *audioError) Error() string             { return e.message }
func audioErr(format string, args ...any) error { return &audioError{fmt.Sprintf(format, args...)} }

type audioInfo struct {
	MIME       string
	Codec      string
	Channels   *int
	SampleRate *int
	DurationMS *int
}

func (i audioInfo) Ext() string { return audioExtensions[i.MIME] }
func (i audioInfo) asMeta() map[string]any {
	meta := map[string]any{"codec": i.Codec}
	if i.Channels != nil {
		meta["channels"] = *i.Channels
	}
	if i.SampleRate != nil {
		meta["sample_rate"] = *i.SampleRate
	}
	if i.DurationMS != nil {
		meta["duration_ms"] = *i.DurationMS
	}
	return meta
}

func looksLikeAudio(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	head := data[:minIntInt(16, len(data))]
	if bytes.HasPrefix(head, []byte("ID3")) || bytes.HasPrefix(head, []byte("OggS")) ||
		bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return true
	}
	if len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && string(head[8:12]) == "WAVE" {
		return true
	}
	if head[0] == 0xff && head[1]&0xe0 == 0xe0 {
		return true
	}
	return len(head) >= 12 && string(head[4:8]) == "ftyp" && audioBrands[string(head[8:12])]
}

func parseAudio(data []byte) (audioInfo, error) {
	if len(data) < 4 {
		return audioInfo{}, audioErr("The file is not a supported audio file.")
	}
	head := data[:minIntInt(16, len(data))]
	switch {
	case bytes.HasPrefix(head, []byte("OggS")):
		return parseOgg(data)
	case len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && string(head[8:12]) == "WAVE":
		return parseWAV(data)
	case len(head) >= 8 && string(head[4:8]) == "ftyp":
		return parseISOBMFF(data)
	case bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return parseWebM(data)
	case bytes.HasPrefix(head, []byte("ID3")) || (head[0] == 0xff && head[1]&0xe0 == 0xe0):
		return parseMP3(data)
	}
	return audioInfo{}, audioErr("The file is not a supported audio file.")
}

// ---- MP3 ---------------------------------------------------------------------------------------

type mp3Frame struct {
	length, sampleRate, channels, samples, layer, sideInfoEnd int
}

var mpegBitrates = map[int][]int{
	11: {32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
	12: {32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
	13: {32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	21: {32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
	22: {8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	23: {8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
}
var mpegRates = map[int][]int{1: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 25: {11025, 12000, 8000}}

func mp3FrameAt(data []byte, pos int) *mp3Frame {
	if pos+4 > len(data) {
		return nil
	}
	b1, b2, b3, b4 := data[pos], data[pos+1], data[pos+2], data[pos+3]
	if b1 != 0xff || b2&0xe0 != 0xe0 {
		return nil
	}
	vbits, lbits := (b2>>3)&3, (b2>>1)&3
	if vbits == 1 || lbits == 0 {
		return nil
	}
	version := map[int]int{3: 1, 2: 2, 0: 25}[int(vbits)]
	layer := 4 - int(lbits)
	bitrateIndex, rateIndex, padding := int(b3>>4), int((b3>>2)&3), int((b3>>1)&1)
	if bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return nil
	}
	key := (version%3)*10 + layer
	if version == 2 {
		key = 20 + layer
	}
	table := mpegBitrates[key]
	if table == nil {
		return nil
	}
	bitrate := table[bitrateIndex-1] * 1000
	sampleRate := mpegRates[version][rateIndex]
	channels := 2
	if (b4 >> 6) == 3 {
		channels = 1
	}
	var length, samples int
	if layer == 1 {
		length, samples = (12*bitrate/sampleRate+padding)*4, 384
	} else if layer == 3 && version != 1 {
		length, samples = 72*bitrate/sampleRate+padding, 576
	} else {
		length, samples = 144*bitrate/sampleRate+padding, 1152
	}
	if length < 8 {
		return nil
	}
	side := 13
	if version == 1 {
		if channels == 2 {
			side = 36
		} else {
			side = 21
		}
	} else if channels == 2 {
		side = 21
	}
	return &mp3Frame{length, sampleRate, channels, samples, layer, pos + side}
}

func skipID3(data []byte) (int, error) {
	pos := 0
	for pos+3 <= len(data) && string(data[pos:pos+3]) == "ID3" {
		if len(data) < pos+10 {
			return 0, audioErr("The ID3 tag is cut short.")
		}
		sizeBytes := data[pos+6 : pos+10]
		if sizeBytes[0]&0x80 != 0 || sizeBytes[1]&0x80 != 0 || sizeBytes[2]&0x80 != 0 || sizeBytes[3]&0x80 != 0 {
			return 0, audioErr("The ID3 tag has an invalid size.")
		}
		size := int(sizeBytes[0])<<21 | int(sizeBytes[1])<<14 | int(sizeBytes[2])<<7 | int(sizeBytes[3])
		footer := 0
		if data[pos+5]&0x10 != 0 {
			footer = 10
		}
		if size > audioMaxID3Bytes {
			return 0, audioErr("The ID3 tag is too large.")
		}
		pos += 10 + size + footer
	}
	return pos, nil
}

func parseMP3(data []byte) (audioInfo, error) {
	pos, err := skipID3(data)
	if err != nil {
		return audioInfo{}, err
	}
	limit := minIntInt(len(data), pos+64*1024)
	for pos < limit && data[pos] == 0 {
		pos++
	}
	first := mp3FrameAt(data, pos)
	if first == nil {
		return audioInfo{}, audioErr("No MP3 audio frame follows the header.")
	}
	frames, cursor := 0, pos
	for cursor < len(data) && frames < audioMaxFrameWalk {
		frame := mp3FrameAt(data, cursor)
		if frame == nil {
			break
		}
		if cursor+frame.length > len(data) {
			if frames == 0 {
				return audioInfo{}, audioErr("The MP3 is cut short.")
			}
			break
		}
		cursor += frame.length
		frames++
	}
	rest := data[cursor:]
	tagged := len(rest) >= 3 && (string(rest[:3]) == "TAG" || (len(rest) >= 8 && string(rest[:8]) == "APETAGEX") || (len(rest) >= 9 && string(rest[:9]) == "LYRICSBEG"))
	if !tagged && float64(cursor-pos) < 0.8*float64(len(data)-pos) {
		return audioInfo{}, audioErr("The MP3 frames are not consecutive.")
	}
	samples := frames * first.samples
	if first.sideInfoEnd+12 <= len(data) {
		tag := string(data[first.sideInfoEnd : first.sideInfoEnd+4])
		if tag == "Xing" || tag == "Info" {
			flags := binary.BigEndian.Uint32(data[first.sideInfoEnd+4 : first.sideInfoEnd+8])
			declared := int64(binary.BigEndian.Uint32(data[first.sideInfoEnd+8:first.sideInfoEnd+12])) * int64(first.samples)
			if flags&1 != 0 && declared*1000 <= int64(audioMaxDurationMS)*int64(first.sampleRate) {
				samples = int(declared)
			}
		}
	}
	ms := int(math.Round(float64(samples) * 1000 / float64(first.sampleRate)))
	ch, rate := first.channels, first.sampleRate
	return audioInfo{MIME: audioMP3MIME, Codec: "mp3", Channels: &ch, SampleRate: &rate, DurationMS: &ms}, nil
}

// ---- Ogg ---------------------------------------------------------------------------------------

var oggCRC = func() [256]uint32 {
	var table [256]uint32
	for i := 0; i < 256; i++ {
		value := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if value&0x80000000 != 0 {
				value = (value << 1) ^ 0x04c11db7
			} else {
				value <<= 1
			}
		}
		table[i] = value
	}
	return table
}()

func oggCRC32(page []byte) uint32 {
	crc := uint32(0)
	for _, b := range page {
		crc = (crc << 8) ^ oggCRC[byte(crc>>24)^b]
	}
	return crc
}

func oggPage(data []byte, pos int) (header byte, granule int64, end int, packet []byte, ok bool) {
	if pos+27 > len(data) || string(data[pos:pos+4]) != "OggS" || data[pos+4] != 0 {
		return
	}
	segments := int(data[pos+26])
	tableEnd := pos + 27 + segments
	if tableEnd > len(data) {
		return
	}
	body := 0
	for _, b := range data[pos+27 : tableEnd] {
		body += int(b)
	}
	end = tableEnd + body
	if end > len(data) {
		return
	}
	granule = int64(binary.LittleEndian.Uint64(data[pos+6 : pos+14]))
	packetEnd := minIntInt(end, tableEnd+64)
	return data[pos+5], granule, end, data[tableEnd:packetEnd], true
}

func parseOgg(data []byte) (audioInfo, error) {
	header, _, firstEnd, packet, ok := oggPage(data, 0)
	if !ok || header&0x02 == 0 {
		return audioInfo{}, audioErr("The Ogg file does not start with a first page.")
	}
	stored := binary.LittleEndian.Uint32(data[22:26])
	check := append([]byte(nil), data[:firstEnd]...)
	copy(check[22:26], []byte{0, 0, 0, 0})
	if oggCRC32(check) != stored {
		return audioInfo{}, audioErr("The first Ogg page is damaged.")
	}
	codec, channels, rate, divisor, preSkip := "", 0, 0, 0, 0
	switch {
	case bytes.HasPrefix(packet, []byte("OpusHead")) && len(packet) >= 19 && packet[8]>>4 == 0 && packet[9] > 0:
		codec, channels, rate, divisor = "opus", int(packet[9]), 48000, 48000
		preSkip = int(binary.LittleEndian.Uint16(packet[10:12]))
	case bytes.HasPrefix(packet, []byte("\x01vorbis")) && len(packet) >= 16:
		codec, channels, rate = "vorbis", int(packet[11]), int(binary.LittleEndian.Uint32(packet[12:16]))
		divisor = rate
		if channels == 0 || rate < 4000 || rate > 192000 {
			return audioInfo{}, audioErr("The Vorbis header is not valid.")
		}
	case bytes.HasPrefix(packet, []byte("\x7fFLAC")) && len(packet) >= 13:
		codec, divisor = "flac", 0
	default:
		return audioInfo{}, audioErr("Only Opus, Vorbis and FLAC audio in Ogg is supported.")
	}
	pos, pages, lastGranule, sawSecond := firstEnd, 1, int64(-1), false
	for pos < len(data) && pages < audioMaxFrameWalk {
		h, granule, end, _, ok := oggPage(data, pos)
		if !ok {
			return audioInfo{}, audioErr("The Ogg file is damaged or cut short.")
		}
		if !sawSecond {
			sawSecond = true
			if h&0x02 != 0 {
				return audioInfo{}, audioErr("The Ogg file holds more than one stream.")
			}
		}
		if granule >= 0 {
			lastGranule = granule
		}
		pos, pages = end, pages+1
	}
	if !sawSecond {
		return audioInfo{}, audioErr("The Ogg file has no audio data.")
	}
	var duration *int
	if divisor > 0 && lastGranule > int64(preSkip) {
		ms := int(math.Round(float64(lastGranule-int64(preSkip)) * 1000 / float64(divisor)))
		duration = &ms
	}
	ch, rt := channels, rate
	var chp, rtp *int
	if ch > 0 {
		chp = &ch
	}
	if rt > 0 {
		rtp = &rt
	}
	return audioInfo{MIME: audioOGGMIME, Codec: codec, Channels: chp, SampleRate: rtp, DurationMS: duration}, nil
}

// ---- WAVE --------------------------------------------------------------------------------------

var wavTags = map[uint16]string{1: "pcm", 3: "float", 6: "alaw", 7: "mulaw", 0xfffe: "pcm"}

func parseWAV(data []byte) (audioInfo, error) {
	pos, chunks := 12, 0
	var fmtTag, channels, rate, byteRate, bits uint32
	var dataSize int64 = -1
	for pos+8 <= len(data) && chunks < audioMaxChunks {
		id := string(data[pos : pos+4])
		size := int64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		if id == "fmt " {
			if size < 16 || body+16 > len(data) {
				return audioInfo{}, audioErr("The WAVE format chunk is damaged.")
			}
			fmtTag = uint32(binary.LittleEndian.Uint16(data[body : body+2]))
			channels = uint32(binary.LittleEndian.Uint16(data[body+2 : body+4]))
			rate = binary.LittleEndian.Uint32(data[body+4 : body+8])
			byteRate = binary.LittleEndian.Uint32(data[body+8 : body+12])
			bits = uint32(binary.LittleEndian.Uint16(data[body+14 : body+16]))
			if fmtTag == 0xfffe && size >= 26 && body+26 <= len(data) {
				fmtTag = uint32(binary.LittleEndian.Uint16(data[body+24 : body+26]))
			}
		} else if id == "data" {
			dataSize = size
			if int64(len(data)-body) < dataSize {
				dataSize = int64(len(data) - body)
			}
			break
		}
		pos = body + int(size) + int(size&1)
		chunks++
	}
	if fmtTag == 0 || dataSize <= 0 || channels < 1 || channels > 8 || rate < 8000 || rate > 192000 ||
		(bits != 8 && bits != 16 && bits != 24 && bits != 32 && bits != 64) || byteRate == 0 {
		return audioInfo{}, audioErr("The WAVE file has no format or no audio data.")
	}
	codec, ok := wavTags[uint16(fmtTag)]
	if !ok {
		return audioInfo{}, audioErr("The WAVE format is not supported.")
	}
	ms := int(math.Round(float64(dataSize) * 1000 / float64(byteRate)))
	ch, r := int(channels), int(rate)
	return audioInfo{MIME: audioWAVMIME, Codec: codec, Channels: &ch, SampleRate: &r, DurationMS: &ms}, nil
}

// ---- ISO base media (M4A) ----------------------------------------------------------------------

type isoBox struct {
	kind      string
	body, end int
}

func isoBoxes(data []byte, start, end int, budget *int) ([]isoBox, error) {
	var out []isoBox
	pos := start
	for pos+8 <= end {
		*budget--
		if *budget < 0 {
			return nil, audioErr("The media file has too many boxes.")
		}
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		kind := string(data[pos+4 : pos+8])
		header := 8
		if size == 1 {
			if pos+16 > end {
				return nil, audioErr("A box is cut short.")
			}
			size = int(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
			header = 16
		} else if size == 0 {
			size = end - pos
		}
		if size < header || pos+size > end {
			return nil, audioErr("A box is larger than its container.")
		}
		out = append(out, isoBox{kind, pos + header, pos + size})
		pos += size
	}
	return out, nil
}

func parseISOBMFF(data []byte) (audioInfo, error) {
	brand := string(data[8:12])
	if !audioBrands[brand] {
		return audioInfo{}, audioErr("The media file is not an audio file.")
	}
	budget := audioMaxBoxes
	boxes, err := isoBoxes(data, 0, len(data), &budget)
	if err != nil {
		return audioInfo{}, err
	}
	seen := map[string]bool{}
	var moov *isoBox
	for i := range boxes {
		seen[boxes[i].kind] = true
		if boxes[i].kind == "moov" {
			b := boxes[i]
			moov = &b
		}
	}
	if moov == nil || (!seen["mdat"] && !seen["moof"]) {
		return audioInfo{}, audioErr("The media file has no audio data.")
	}
	handlers := map[string]bool{}
	var duration *int
	var walk func(start, end, depth int) error
	walk = func(start, end, depth int) error {
		if depth > audioMaxBoxDepth {
			return audioErr("The media boxes are nested too deeply.")
		}
		items, err := isoBoxes(data, start, end, &budget)
		if err != nil {
			return err
		}
		for _, box := range items {
			switch box.kind {
			case "trak", "mdia":
				if err := walk(box.body, box.end, depth+1); err != nil {
					return err
				}
			case "hdlr":
				if box.end-box.body >= 12 {
					handlers[string(data[box.body+8:box.body+12])] = true
				}
			case "mvhd":
				if depth == 0 && duration == nil {
					duration = mvhdDuration(data, box.body, box.end)
				}
			}
		}
		return nil
	}
	if err := walk(moov.body, moov.end, 0); err != nil {
		return audioInfo{}, err
	}
	if handlers["vide"] || handlers["auxv"] {
		return audioInfo{}, audioErr("The media file contains video, not just audio.")
	}
	if !handlers["soun"] {
		return audioInfo{}, audioErr("The media file has no audio track.")
	}
	return audioInfo{MIME: audioM4AMIME, Codec: "aac", DurationMS: duration}, nil
}

func mvhdDuration(data []byte, body, end int) *int {
	if end-body < 20 {
		return nil
	}
	var timescale, duration uint64
	switch data[body] {
	case 1:
		if end-body >= 32 {
			timescale = uint64(binary.BigEndian.Uint32(data[body+20 : body+24]))
			duration = binary.BigEndian.Uint64(data[body+24 : body+32])
		}
	case 0:
		timescale = uint64(binary.BigEndian.Uint32(data[body+12 : body+16]))
		duration = uint64(binary.BigEndian.Uint32(data[body+16 : body+20]))
	default:
		return nil
	}
	if timescale == 0 || duration == 0xffffffff || duration == 0xffffffffffffffff {
		return nil
	}
	ms := int(math.Round(float64(duration) * 1000 / float64(timescale)))
	return &ms
}

// ---- WebM --------------------------------------------------------------------------------------

func ebmlVint(data []byte, pos int, keepMarker bool) (value, length int, unknown, ok bool) {
	if pos >= len(data) {
		return 0, 0, false, false
	}
	first := data[pos]
	length = 1
	for length <= 8 && first&(0x80>>(length-1)) == 0 {
		length++
	}
	if length > 8 || pos+length > len(data) {
		return 0, 0, false, false
	}
	value = int(first)
	if !keepMarker {
		value = int(first & byte(0xff>>length))
	}
	for _, b := range data[pos+1 : pos+length] {
		value = value<<8 | int(b)
	}
	if !keepMarker && value == (1<<(7*length))-1 {
		unknown = true
	}
	return value, length, unknown, true
}

func parseWebM(data []byte) (audioInfo, error) {
	if len(data) > audioEBMLScanBytes {
		data = data[:audioEBMLScanBytes]
	}
	trackTypes := map[int]bool{}
	codecs := map[string]bool{}
	doctype := ""
	scale := uint64(1_000_000)
	var duration float64
	elements := 0
	var durSet bool
	var walk func(start, end, depth int) bool
	walk = func(start, end, depth int) bool {
		pos := start
		for pos < end {
			elements++
			if elements > audioMaxEBMLElements || depth > audioMaxBoxDepth {
				return true
			}
			id, idLen, _, ok := ebmlVint(data, pos, true)
			if !ok {
				return true
			}
			size, sizeLen, unknown, ok := ebmlVint(data, pos+idLen, false)
			if !ok {
				return true
			}
			body := pos + idLen + sizeLen
			bodyEnd := end
			if unknown {
				bodyEnd = end
			} else if body+size < end {
				bodyEnd = body + size
			}
			switch id {
			case 0x1f43b675:
				return false
			case 0x1a45dfa3, 0x18538067, 0x1549a966, 0x1654ae6b, 0xae:
				if !walk(body, bodyEnd, depth+1) {
					return false
				}
			case 0x4282:
				doctype = string(data[body:bodyEnd])
			case 0x83:
				if bodyEnd > body {
					trackTypes[int(intFromBytes(data[body:bodyEnd]))] = true
				}
			case 0x86:
				codecs[string(data[body:bodyEnd])] = true
			case 0x2ad7b1:
				if bodyEnd > body {
					scale = uint64(intFromBytes(data[body:bodyEnd]))
					if scale == 0 {
						scale = 1_000_000
					}
				}
			case 0x4489:
				if bodyEnd-body == 4 {
					duration = float64(math.Float32frombits(binary.BigEndian.Uint32(data[body:bodyEnd])))
					durSet = true
				} else if bodyEnd-body == 8 {
					duration = math.Float64frombits(binary.BigEndian.Uint64(data[body:bodyEnd]))
					durSet = true
				}
			}
			if unknown {
				return true
			}
			pos = bodyEnd
		}
		return true
	}
	walk(0, len(data), 0)
	if doctype != "webm" {
		return audioInfo{}, audioErr("The file is not a WebM file.")
	}
	if trackTypes[1] || trackTypes[3] {
		return audioInfo{}, audioErr("The WebM file contains video, not just audio.")
	}
	if !trackTypes[2] || (!codecs["A_OPUS"] && !codecs["A_VORBIS"]) {
		return audioInfo{}, audioErr("The WebM file has no Opus or Vorbis audio track.")
	}
	codec := "vorbis"
	if codecs["A_OPUS"] {
		codec = "opus"
	}
	var ms *int
	if durSet && duration > 0 {
		value := int(math.Round(duration * float64(scale) / 1_000_000))
		ms = &value
	}
	return audioInfo{MIME: audioWebMMIME, Codec: codec, DurationMS: ms}, nil
}

func intFromBytes(data []byte) int64 {
	var value int64
	for _, b := range data {
		value = value<<8 | int64(b)
	}
	return value
}
