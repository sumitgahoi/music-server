package inventory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReadStoreIDs(t *testing.T) {
	body := purchaseFile(1744253545, 1744253558, 0)
	ids, err := readStoreIDs(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if ids.Track != 1744253558 || ids.Album != 1744253545 {
		t.Fatalf("ids = %+v", ids)
	}
}

func TestStoreCatalogSharesOneAlbumDownload(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	var lookups, images int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lookup":
			lookups++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resultCount":1,"results":[{"artworkUrl100":"https://` + r.Host + `/art/100x100bb.jpg"}]}`))
		case "/art/600x600bb.jpg":
			images++
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(picture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cat := newStoreCatalog(srv.Client())
	cat.lookupURL = func(id int64) string {
		return srv.URL + "/lookup?id=" + strconv.FormatInt(id, 10)
	}
	cat.allowHost = func(string) bool { return true }

	art, done, err := cat.Cover(99, 7)
	if err != nil || !done || art == nil || !bytes.Equal(art.Data, picture) || art.MIME != "image/jpeg" {
		t.Fatalf("art=%v done=%v err=%v", art, done, err)
	}
	if _, _, err := cat.Cover(99, 8); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 || images != 1 {
		t.Fatalf("lookups=%d images=%d", lookups, images)
	}

	empty := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups++
		_, _ = w.Write([]byte(`{"resultCount":0,"results":[]}`))
	}))
	defer empty.Close()
	miss := newStoreCatalog(empty.Client())
	miss.lookupURL = func(int64) string { return empty.URL }
	miss.allowHost = func(string) bool { return true }
	lookups = 0
	art, done, err = miss.Cover(5, 1)
	if err != nil || !done || art != nil {
		t.Fatalf("miss art=%v done=%v err=%v", art, done, err)
	}
	_, _, _ = miss.Cover(5, 2)
	if lookups != 1 {
		t.Fatalf("miss lookups=%d", lookups)
	}
}

func TestPurchaseCoverIsStoredOnce(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.m4a"), purchaseFile(99, 7, 'a'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.m4a"), purchaseFile(99, 8, 'b'), 0o644); err != nil {
		t.Fatal(err)
	}
	covers := &fakeStore{art: coverFromBytes(picture, "image/jpeg"), definitive: true}
	s := newStore(t)
	if err := scan(root, s, nil, openFile, covers); err != nil {
		t.Fatal(err)
	}
	if covers.calls != 2 {
		t.Fatalf("calls = %d", covers.calls)
	}
	cat := mustBootstrap(t, s, "ada")
	if len(cat.Songs) != 2 {
		t.Fatalf("songs = %d", len(cat.Songs))
	}
	for _, song := range cat.Songs {
		if song.Artwork != covers.art.Hash {
			t.Fatalf("artwork = %q", song.Artwork)
		}
	}
	var checked int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE store_checked = 1 AND artwork_hash != ''`).Scan(&checked); err != nil {
		t.Fatal(err)
	}
	if checked != 2 {
		t.Fatalf("checked = %d", checked)
	}

	var opens int
	open := func(name string) (io.ReadCloser, error) {
		opens++
		return os.Open(name)
	}
	if err := scan(root, s, nil, open, covers); err != nil {
		t.Fatal(err)
	}
	if opens != 0 || covers.calls != 2 {
		t.Fatalf("opens=%d calls=%d", opens, covers.calls)
	}
}

func TestDeletedStoreCoverRefillsFromCatalog(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	art := coverFromBytes(picture, "image/jpeg")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.m4a"), purchaseFile(99, 7, 'a'), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := scan(root, s, nil, openFile, &fakeStore{art: art, definitive: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.artworkDir(), art.Hash)); err != nil {
		t.Fatal(err)
	}
	open := func(rel string) (io.ReadSeekCloser, error) {
		return os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	}
	if _, _, err := s.openArtwork(art.Hash, open, &fakeStore{err: errors.New("should use embedded first")}); err == nil {
		t.Fatal("expected the purchase file to have no embedded cover")
	}
	refill := &fakeStore{art: art, definitive: true}
	f, mime, err := s.openArtwork(art.Hash, open, refill)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" || !bytes.Equal(got, picture) || refill.calls != 1 {
		t.Fatalf("mime=%s calls=%d body=%x", mime, refill.calls, got)
	}

	if err := os.Remove(filepath.Join(s.artworkDir(), art.Hash)); err != nil {
		t.Fatal(err)
	}
	otherPicture := []byte{0xff, 0xd8, 0xff, 0xd0}
	other := coverFromBytes(otherPicture, "image/jpeg")
	_, _, err = s.openArtwork(art.Hash, open, &fakeStore{art: other, definitive: true})
	if !errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(s.artworkDir(), art.Hash)); !os.IsNotExist(statErr) {
		t.Fatalf("old image was cached: %v", statErr)
	}
	cached, err := os.ReadFile(filepath.Join(s.artworkDir(), other.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, otherPicture) {
		t.Fatalf("new cache = %x", cached)
	}
	cat := mustBootstrap(t, s, "ada")
	if len(cat.Songs) != 1 || cat.Songs[0].Artwork != other.Hash {
		t.Fatalf("artwork = %q", cat.Songs[0].Artwork)
	}
	var payload string
	if err := s.db.QueryRow(`SELECT payload FROM actions WHERE seq = ?`, cat.Seq).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, other.Hash) {
		t.Fatalf("latest action = %s", payload)
	}
	f, mime, err = s.openArtwork(other.Hash, open, &fakeStore{err: errors.New("cache hit")})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if mime != "image/jpeg" {
		t.Fatalf("mime = %s", mime)
	}
}

func TestStoreFailureIsRetried(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.m4a"), purchaseFile(99, 7, 'a'), 0o644); err != nil {
		t.Fatal(err)
	}
	covers := &fakeStore{err: errors.New("down")}
	s := newStore(t)
	if err := scan(root, s, nil, openFile, covers); err != nil {
		t.Fatal(err)
	}
	if covers.calls != 1 {
		t.Fatalf("calls = %d", covers.calls)
	}
	var hash string
	var checked int
	if err := s.db.QueryRow(`SELECT artwork_hash, store_checked FROM assets`).Scan(&hash, &checked); err != nil {
		t.Fatal(err)
	}
	if hash != "" || checked != 0 {
		t.Fatalf("hash=%q checked=%d", hash, checked)
	}
	if err := scan(root, s, nil, openFile, covers); err != nil {
		t.Fatal(err)
	}
	if covers.calls != 2 {
		t.Fatalf("calls = %d", covers.calls)
	}
}

func TestStoreMissIsNotRetried(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.m4a"), purchaseFile(99, 7, 'a'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "song.flac"), []byte("flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	covers := &fakeStore{definitive: true}
	s := newStore(t)
	if err := scan(root, s, nil, openFile, covers); err != nil {
		t.Fatal(err)
	}
	if covers.calls != 1 {
		t.Fatalf("calls = %d", covers.calls)
	}
	var opens int
	open := func(name string) (io.ReadCloser, error) {
		opens++
		return os.Open(name)
	}
	if err := scan(root, s, nil, open, covers); err != nil {
		t.Fatal(err)
	}
	if opens != 0 || covers.calls != 1 {
		t.Fatalf("opens=%d calls=%d", opens, covers.calls)
	}
	cat := mustBootstrap(t, s, "ada")
	for _, song := range cat.Songs {
		if song.Artwork != "" {
			t.Fatalf("artwork = %q", song.Artwork)
		}
	}
}

type fakeStore struct {
	calls      int
	art        *coverArt
	definitive bool
	err        error
}

func openFile(name string) (io.ReadCloser, error) {
	return os.Open(name)
}

func (f *fakeStore) Cover(int64, int64) (*coverArt, bool, error) {
	f.calls++
	return f.art, f.definitive, f.err
}

func purchaseFile(album, track int64, extra byte) []byte {
	ilst := box("ilst", append(
		box("cnID", dataInt(track, 4)),
		box("plID", dataInt(album, 8))...,
	))
	meta := box("meta", append([]byte{0, 0, 0, 0}, ilst...))
	file := append(box("ftyp", []byte("M4A ")), box("moov", box("udta", meta))...)
	if extra != 0 {
		file = append(file, extra)
	}
	return file
}

func dataInt(n int64, width int) []byte {
	payload := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		payload[i] = byte(n)
		n >>= 8
	}
	atom := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(atom[0:4], uint32(len(atom)))
	copy(atom[4:8], "data")
	atom[11] = 21
	copy(atom[16:], payload)
	return atom
}
