package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFrontCoverIsCachedUntilDeleted(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	body := id3v23(
		id3Text("TIT2", "Hello"),
		id3APIC(3, "image/jpeg", picture),
	)
	root := t.TempDir()
	full := filepath.Join(root, "Album", "song.mp3")
	if err := os.Mkdir(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
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

	sum := sha256.Sum256(picture)
	hash := hex.EncodeToString(sum[:])
	cat := mustBootstrap(t, s, "ada")
	if len(cat.Songs) != 1 || cat.Songs[0].Artwork != hash {
		t.Fatalf("artwork = %q", cat.Songs[0].Artwork)
	}
	id := cat.Songs[0].ID
	meta := string(cat.Songs[0].Metadata)
	cached, err := os.ReadFile(filepath.Join(s.artworkDir(), hash))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, picture) {
		t.Fatalf("cache = %x", cached)
	}

	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	again := mustBootstrap(t, s, "ada")
	if again.Songs[0].ID != id || again.Songs[0].Artwork != hash || string(again.Songs[0].Metadata) != meta {
		t.Fatalf("rescan changed song %+v", again.Songs[0])
	}
	if err := os.Remove(filepath.Join(s.artworkDir(), hash)); err != nil {
		t.Fatal(err)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.artworkDir(), hash)); !os.IsNotExist(err) {
		t.Fatalf("rescan rewrote cache: %v", err)
	}

	f, mime, err := s.OpenArtwork(hash, func(rel string) (io.ReadSeekCloser, error) {
		return os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" || !bytes.Equal(got, picture) {
		t.Fatalf("mime %s body %x", mime, got)
	}
}

func TestUnsetArtworkIsReadOnceWithoutChangingIdentity(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	body := id3v23(id3Text("TIT2", "Hello"), id3APIC(3, "image/jpeg", picture))
	root := t.TempDir()
	full := filepath.Join(root, "song.mp3")
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	cat := mustBootstrap(t, s, "ada")
	song := cat.Songs[0]
	if _, err := s.db.Exec(`UPDATE assets SET artwork_hash = NULL, artwork_mime = NULL WHERE id = ?`, song.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.artworkDir(), song.Artwork)); err != nil {
		t.Fatal(err)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	again := mustBootstrap(t, s, "ada")
	if again.Songs[0].ID != song.ID || again.Songs[0].Hash != song.Hash || string(again.Songs[0].Metadata) != string(song.Metadata) {
		t.Fatalf("identity changed: %+v", again.Songs[0])
	}
	if again.Songs[0].Artwork != song.Artwork {
		t.Fatalf("artwork = %q, want %q", again.Songs[0].Artwork, song.Artwork)
	}
	if _, err := os.Stat(filepath.Join(s.artworkDir(), song.Artwork)); err != nil {
		t.Fatal(err)
	}
}

func TestBackCoverIsOmitted(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	body := id3v23(id3APIC(4, "image/jpeg", picture))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	cat := mustBootstrap(t, s, "ada")
	if cat.Songs[0].Artwork != "" {
		t.Fatalf("artwork = %q", cat.Songs[0].Artwork)
	}
}

func TestArtworkUnavailableWhenEveryCopyIsMissing(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	body := id3v23(id3APIC(3, "image/jpeg", picture))
	root := t.TempDir()
	full := filepath.Join(root, "song.mp3")
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	hash := mustBootstrap(t, s, "ada").Songs[0].Artwork
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.artworkDir(), hash)); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.OpenArtwork(hash, func(string) (io.ReadSeekCloser, error) {
		t.Fatal("opened a file")
		return nil, nil
	})
	if !errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("err = %v", err)
	}
	_, _, err = s.OpenArtwork("nope", func(string) (io.ReadSeekCloser, error) {
		t.Fatal("opened a file")
		return nil, nil
	})
	if !errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func id3APIC(picType byte, mime string, data []byte) []byte {
	payload := []byte{0}
	payload = append(payload, mime...)
	payload = append(payload, 0, picType, 0)
	payload = append(payload, data...)
	frame := make([]byte, 10+len(payload))
	copy(frame[0:4], "APIC")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[10:], payload)
	return frame
}
