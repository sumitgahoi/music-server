package inventory

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScanReadsID3WithoutModifyingFile(t *testing.T) {
	body := id3v23(
		id3Text("TIT2", "Hello"),
		id3Text("TPE1", "Ada"),
		id3Text("TALB", "Album"),
		id3Text("TPE2", "Ada"),
		id3Text("TYER", "2023"),
		id3Text("TRCK", "1"),
		id3Text("TPOS", "2"),
		id3Text("TCMP", "1"),
	)
	root := t.TempDir()
	full := filepath.Join(root, "song.mp3")
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("scan modified the file")
	}
	cat := mustBootstrap(t, s, "ada")
	if len(cat.Songs) != 1 {
		t.Fatalf("songs = %d", len(cat.Songs))
	}
	var doc metadataDoc
	if err := json.Unmarshal(cat.Songs[0].Metadata, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Hello" || len(doc.Artists) != 1 || doc.Artists[0] != "Ada" {
		t.Fatalf("metadata = %+v", doc)
	}
	if doc.Album != "Album" || doc.AlbumArtist != "Ada" || doc.Year != 2023 || doc.TrackNumber != 1 || doc.DiscNumber != 2 || !doc.Compilation {
		t.Fatalf("metadata = %+v", doc)
	}
	id := cat.Songs[0].ID
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	again := mustBootstrap(t, s, "ada")
	if again.Songs[0].ID != id || string(again.Songs[0].Metadata) != string(cat.Songs[0].Metadata) {
		t.Fatalf("rescan changed song %s metadata %s", again.Songs[0].ID, again.Songs[0].Metadata)
	}
}

func TestMetadataBackfillKeepsSongID(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "song", ContentID: "alpha", Size: 4})
	id := mustSnapshot(t, s).Assets[0].ID
	meta := `{"title":"Hello","artists":["Ada"]}`
	apply(t, s, "/music", Observation{Path: "song", ContentID: "alpha", Size: 4, Metadata: meta})
	cat := mustBootstrap(t, s, "ada")
	if cat.Songs[0].ID != id || string(cat.Songs[0].Metadata) != meta {
		t.Fatalf("song = %+v", cat.Songs[0])
	}
}

func TestFLACDuration(t *testing.T) {
	si := make([]byte, 34)
	sampleRate := 44100
	si[10] = byte(sampleRate >> 12)
	si[11] = byte(sampleRate >> 4)
	si[12] = byte(sampleRate << 4)
	si[16] = 0xAC
	si[17] = 0x44
	body := append([]byte("fLaC"), 0x80, 0, 0, 34)
	body = append(body, si...)
	got, ok := flacDuration(bytes.NewReader(body))
	if !ok || got != 1000 {
		t.Fatalf("duration = %d ok=%v", got, ok)
	}
}

func TestMP4DurationAndCodec(t *testing.T) {
	mvhd := make([]byte, 28)
	binary.BigEndian.PutUint32(mvhd[0:4], uint32(len(mvhd)))
	copy(mvhd[4:8], "mvhd")
	binary.BigEndian.PutUint32(mvhd[20:24], 1000)
	binary.BigEndian.PutUint32(mvhd[24:28], 2500)
	stsd := make([]byte, 8+8+8)
	binary.BigEndian.PutUint32(stsd[0:4], uint32(len(stsd)))
	copy(stsd[4:8], "stsd")
	binary.BigEndian.PutUint32(stsd[16:20], 8)
	copy(stsd[20:24], "alac")
	stbl := box("stbl", stsd)
	minf := box("minf", stbl)
	mdia := box("mdia", minf)
	trak := box("trak", mdia)
	moov := box("moov", append(mvhd, trak...))
	ftyp := make([]byte, 20)
	binary.BigEndian.PutUint32(ftyp[0:4], 20)
	copy(ftyp[4:8], "ftyp")
	copy(ftyp[8:12], "M4A ")
	copy(ftyp[16:20], "M4A ")
	body := append(ftyp, moov...)
	rs := bytes.NewReader(body)
	ms, codec := mp4Info(rs, int64(len(body)))
	if ms != 2500 || codec != "alac" {
		t.Fatalf("duration=%d codec=%s", ms, codec)
	}
}

func box(kind string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], kind)
	copy(out[8:], payload)
	return out
}

func id3v23(frames ...[]byte) []byte {
	var body []byte
	for _, frame := range frames {
		body = append(body, frame...)
	}
	hdr := make([]byte, 10+len(body))
	copy(hdr, "ID3")
	hdr[3] = 3
	n := len(body)
	hdr[6] = byte((n >> 21) & 0x7f)
	hdr[7] = byte((n >> 14) & 0x7f)
	hdr[8] = byte((n >> 7) & 0x7f)
	hdr[9] = byte(n & 0x7f)
	copy(hdr[10:], body)
	return hdr
}

func id3Text(id, text string) []byte {
	data := append([]byte{0}, text...)
	frame := make([]byte, 10+len(data))
	copy(frame[0:4], id)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(data)))
	copy(frame[10:], data)
	return frame
}
