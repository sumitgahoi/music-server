package inventory

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dhowden/tag"
)

// coverArt is the front-cover image extracted while tags are read.
// The bytes are cached by Hash; they are not stored in song metadata.
type coverArt struct {
	Hash string
	MIME string
	Data []byte
}

// metadataDoc is the device view of a song. Paths and embedded artwork
// are not included. Empty fields are omitted.
type metadataDoc struct {
	Title       string   `json:"title,omitempty"`
	Artists     []string `json:"artists,omitempty"`
	Album       string   `json:"album,omitempty"`
	AlbumArtist string   `json:"albumArtist,omitempty"`
	Compilation bool     `json:"compilation,omitempty"`
	TrackNumber int      `json:"trackNumber,omitempty"`
	DiscNumber  int      `json:"discNumber,omitempty"`
	Year        int      `json:"year,omitempty"`
	DurationMs  int64    `json:"durationMs,omitempty"`
	Codec       string   `json:"codec,omitempty"`
	Bitrate     int      `json:"bitrate,omitempty"`
}

// extractMetadata reads tags and container headers. A file with no
// readable tags returns {}. The cover is the front image from the same
// tag read, or nil when the file has none.
func extractMetadata(r io.ReadSeeker, size int64) (string, *coverArt, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "{}", nil, err
	}
	doc := metadataDoc{}
	var art *coverArt
	meta, err := tag.ReadFrom(r)
	if err != nil && !errors.Is(err, tag.ErrNoTagsFound) {
		return "{}", nil, err
	}
	if err == nil {
		doc = docFromTag(meta)
		art = coverFromTag(meta)
	}
	if _, seekErr := r.Seek(0, io.SeekStart); seekErr != nil {
		return "{}", nil, seekErr
	}
	durationMs, codec := mediaInfo(r, size)
	if doc.Codec == "" {
		doc.Codec = codec
	}
	if durationMs > 0 {
		doc.DurationMs = durationMs
		if size > 0 {
			doc.Bitrate = int(size * 8 * 1000 / durationMs)
		}
	}
	if doc.empty() {
		return "{}", art, nil
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "{}", nil, err
	}
	return string(raw), art, nil
}

// readCover reads the front cover and leaves other tag fields alone.
// A file with no tags returns a nil cover and a nil error.
func readCover(r io.ReadSeeker) (*coverArt, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	meta, err := tag.ReadFrom(r)
	if errors.Is(err, tag.ErrNoTagsFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return coverFromTag(meta), nil
}

// coverFromTag keeps the front cover. An MP4 cover atom has no picture
// type, and a lone ID3 image is often type Other, so those count too.
// A back cover, icon, or artist image is left out.
func coverFromTag(meta tag.Metadata) *coverArt {
	if meta == nil {
		return nil
	}
	picture := meta.Picture()
	if picture == nil || len(picture.Data) == 0 || !frontCover(picture.Type) {
		return nil
	}
	mime := imageMIME(picture.MIMEType, picture.Data)
	if mime == "" {
		return nil
	}
	sum := sha256.Sum256(picture.Data)
	return &coverArt{
		Hash: hex.EncodeToString(sum[:]),
		MIME: mime,
		Data: append([]byte(nil), picture.Data...),
	}
}

func frontCover(kind string) bool {
	switch kind {
	case "", "Cover (front)", "Other":
		return true
	default:
		return false
	}
}

func imageMIME(declared string, data []byte) string {
	mime := canonicalImageMIME(declared)
	if mime != "" {
		return mime
	}
	return canonicalImageMIME(http.DetectContentType(data))
}

func canonicalImageMIME(raw string) string {
	mime := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "image/jpg" {
		mime = "image/jpeg"
	}
	if !strings.HasPrefix(mime, "image/") || strings.ContainsAny(mime, " \r\n") {
		return ""
	}
	return mime
}

func (d metadataDoc) empty() bool {
	return d.Title == "" && len(d.Artists) == 0 && d.Album == "" && d.AlbumArtist == "" &&
		!d.Compilation && d.TrackNumber == 0 && d.DiscNumber == 0 && d.Year == 0 &&
		d.DurationMs == 0 && d.Codec == "" && d.Bitrate == 0
}

func docFromTag(meta tag.Metadata) metadataDoc {
	doc := metadataDoc{
		Title:       strings.TrimSpace(meta.Title()),
		Album:       strings.TrimSpace(meta.Album()),
		AlbumArtist: strings.TrimSpace(meta.AlbumArtist()),
		Codec:       codecOf(meta.FileType()),
		Compilation: compilationOf(meta.Raw()),
	}
	if artist := strings.TrimSpace(meta.Artist()); artist != "" {
		doc.Artists = []string{artist}
	}
	if track, _ := meta.Track(); track > 0 {
		doc.TrackNumber = track
	}
	if disc, _ := meta.Disc(); disc > 0 {
		doc.DiscNumber = disc
	}
	if year := meta.Year(); year > 0 {
		doc.Year = year
	}
	return doc
}

func codecOf(ft tag.FileType) string {
	switch ft {
	case tag.MP3:
		return "mp3"
	case tag.M4A, tag.M4B, tag.M4P:
		return "aac"
	case tag.ALAC:
		return "alac"
	case tag.FLAC:
		return "flac"
	case tag.OGG:
		return "vorbis"
	case tag.DSF:
		return "dsd"
	default:
		return ""
	}
}

func compilationOf(raw map[string]interface{}) bool {
	for key, value := range raw {
		switch strings.ToLower(key) {
		case "tcmp", "cpil", "compilation":
			return truthy(value)
		}
	}
	return false
}

func truthy(value interface{}) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v == "1" || strings.EqualFold(v, "true")
	case int:
		return v != 0
	case int64:
		return v != 0
	default:
		return false
	}
}

func mediaInfo(r io.ReadSeeker, size int64) (int64, string) {
	var magic [12]byte
	n, err := io.ReadFull(r, magic[:])
	if err != nil && n < 4 {
		return 0, ""
	}
	switch {
	case string(magic[0:4]) == "fLaC":
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return 0, "flac"
		}
		ms, ok := flacDuration(r)
		if !ok {
			return 0, "flac"
		}
		return ms, "flac"
	case n >= 8 && string(magic[4:8]) == "ftyp":
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return 0, ""
		}
		return mp4Info(r, size)
	case string(magic[0:3]) == "ID3" || (magic[0] == 0xFF && magic[1]&0xE0 == 0xE0):
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return 0, ""
		}
		ms, ok := mp3Duration(r, size)
		if !ok {
			return 0, ""
		}
		return ms, "mp3"
	default:
		return 0, ""
	}
}

func flacDuration(r io.Reader) (int64, bool) {
	var mark [4]byte
	if _, err := io.ReadFull(r, mark[:]); err != nil || string(mark[:]) != "fLaC" {
		return 0, false
	}
	var block [4]byte
	if _, err := io.ReadFull(r, block[:]); err != nil {
		return 0, false
	}
	if block[0]&0x7f != 0 {
		return 0, false
	}
	length := int(block[1])<<16 | int(block[2])<<8 | int(block[3])
	if length < 18 {
		return 0, false
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, false
	}
	sampleRate := uint32(buf[10])<<12 | uint32(buf[11])<<4 | uint32(buf[12])>>4
	samples := uint64(buf[13]&0x0f)<<32 | uint64(buf[14])<<24 | uint64(buf[15])<<16 | uint64(buf[16])<<8 | uint64(buf[17])
	if sampleRate == 0 || samples == 0 {
		return 0, false
	}
	return int64(samples) * 1000 / int64(sampleRate), true
}

func mp4Info(r io.ReadSeeker, size int64) (int64, string) {
	codec := "aac"
	var duration int64
	var pos int64
	for pos+8 <= size {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return duration, codec
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return duration, codec
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
		kind := string(hdr[4:8])
		header := int64(8)
		if boxSize == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return duration, codec
			}
			boxSize = int64(binary.BigEndian.Uint64(ext[:]))
			header = 16
		} else if boxSize == 0 {
			boxSize = size - pos
		}
		if boxSize < header || pos+boxSize > size {
			return duration, codec
		}
		if kind == "moov" {
			d, c := walkMP4(r, pos+header, pos+boxSize)
			if d > 0 {
				duration = d
			}
			if c != "" {
				codec = c
			}
			return duration, codec
		}
		pos += boxSize
	}
	return duration, codec
}

func walkMP4(r io.ReadSeeker, start, end int64) (int64, string) {
	var duration int64
	var codec string
	for pos := start; pos+8 <= end; {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return duration, codec
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return duration, codec
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
		kind := string(hdr[4:8])
		header := int64(8)
		if boxSize == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return duration, codec
			}
			boxSize = int64(binary.BigEndian.Uint64(ext[:]))
			header = 16
		}
		if boxSize < header || pos+boxSize > end {
			return duration, codec
		}
		payload := pos + header
		switch kind {
		case "mvhd":
			if ms, ok := readMvhd(r, payload); ok {
				duration = ms
			}
		case "stsd":
			if c := readSTSD(r, payload, pos+boxSize); c != "" {
				codec = c
			}
		case "moov", "trak", "mdia", "minf", "stbl":
			d, c := walkMP4(r, payload, pos+boxSize)
			if d > 0 {
				duration = d
			}
			if c != "" {
				codec = c
			}
		}
		pos += boxSize
	}
	return duration, codec
}

func readMvhd(r io.ReadSeeker, payload int64) (int64, bool) {
	if _, err := r.Seek(payload, io.SeekStart); err != nil {
		return 0, false
	}
	var vf [4]byte
	if _, err := io.ReadFull(r, vf[:]); err != nil {
		return 0, false
	}
	if vf[0] == 0 {
		var buf [16]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, false
		}
		timescale := binary.BigEndian.Uint32(buf[8:12])
		dur := binary.BigEndian.Uint32(buf[12:16])
		if timescale == 0 {
			return 0, false
		}
		return int64(dur) * 1000 / int64(timescale), true
	}
	var buf [28]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, false
	}
	timescale := binary.BigEndian.Uint32(buf[16:20])
	dur := binary.BigEndian.Uint64(buf[20:28])
	if timescale == 0 {
		return 0, false
	}
	return int64(dur) * 1000 / int64(timescale), true
}

func readSTSD(r io.ReadSeeker, payload, end int64) string {
	if payload+16 > end {
		return ""
	}
	// version/flags, entry count, then the first sample entry's size and codec.
	if _, err := r.Seek(payload+12, io.SeekStart); err != nil {
		return ""
	}
	var kind [4]byte
	if _, err := io.ReadFull(r, kind[:]); err != nil {
		return ""
	}
	switch string(kind[:]) {
	case "mp4a":
		return "aac"
	case "alac":
		return "alac"
	case "fLaC", "flac":
		return "flac"
	case "Opus":
		return "opus"
	default:
		return ""
	}
}

func mp3Duration(r io.ReadSeeker, size int64) (int64, bool) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, false
	}
	audioStart := int64(0)
	var hdr [10]byte
	if _, err := io.ReadFull(r, hdr[:]); err == nil && string(hdr[0:3]) == "ID3" && hdr[3] <= 4 {
		audioStart = 10 + (int64(hdr[6]&0x7f)<<21 | int64(hdr[7]&0x7f)<<14 | int64(hdr[8]&0x7f)<<7 | int64(hdr[9]&0x7f))
	}
	if audioStart < 0 || audioStart >= size {
		return 0, false
	}
	if _, err := r.Seek(audioStart, io.SeekStart); err != nil {
		return 0, false
	}
	buf := make([]byte, 16*1024)
	n, err := io.ReadFull(r, buf)
	if err != nil && n < 4 {
		return 0, false
	}
	buf = buf[:n]
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] != 0xFF || buf[i+1]&0xE0 != 0xE0 {
			continue
		}
		h := binary.BigEndian.Uint32(buf[i : i+4])
		bitrate, sampleRate, ok := mpeg1Layer3(h)
		if !ok {
			continue
		}
		if frames, ok := xingFrames(buf[i:], h); ok && sampleRate > 0 {
			return int64(frames) * 1152 * 1000 / int64(sampleRate), true
		}
		audioBytes := size - (audioStart + int64(i))
		if audioBytes <= 0 {
			return 0, false
		}
		return audioBytes * 8 * 1000 / int64(bitrate), true
	}
	return 0, false
}

func mpeg1Layer3(h uint32) (bitrate, sampleRate int, ok bool) {
	if h&0xFFE00000 != 0xFFE00000 {
		return 0, 0, false
	}
	if (h>>19)&3 != 3 || (h>>17)&3 != 1 {
		return 0, 0, false
	}
	bitrateIdx := (h >> 12) & 0xF
	rateIdx := (h >> 10) & 3
	rates := [...]int{0, 32000, 40000, 48000, 56000, 64000, 80000, 96000, 112000, 128000, 160000, 192000, 224000, 256000, 320000, 0}
	samples := [...]int{44100, 48000, 32000, 0}
	if bitrateIdx == 0 || bitrateIdx == 15 || rateIdx == 3 {
		return 0, 0, false
	}
	return rates[bitrateIdx], samples[rateIdx], true
}

func xingFrames(frame []byte, h uint32) (uint32, bool) {
	side := 32
	if (h>>6)&3 == 3 {
		side = 17
	}
	off := 4 + side
	if off+12 > len(frame) {
		return 0, false
	}
	tag := string(frame[off : off+4])
	if tag != "Xing" && tag != "Info" {
		return 0, false
	}
	flags := binary.BigEndian.Uint32(frame[off+4 : off+8])
	if flags&1 == 0 {
		return 0, false
	}
	return binary.BigEndian.Uint32(frame[off+8 : off+12]), true
}
