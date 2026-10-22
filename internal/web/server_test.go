package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"music-server/internal/inventory"
)

func TestHealth(t *testing.T) {
	store := openStore(t)
	ts, _ := serve(t, t.TempDir(), store)

	res, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %s", ct)
	}
	var body healthDocument
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK {
		t.Fatalf("health = %+v", body)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %s", res.Status)
	}
}

func TestInventoryJSONAndQuietRename(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)

	doc := getInventory(t, ts.URL+"/inventory.json")
	if doc.Root != root || len(doc.Assets) != 1 {
		t.Fatalf("doc root=%s assets=%d", doc.Root, len(doc.Assets))
	}
	id := doc.Assets[0].ID
	if id == "" || doc.Assets[0].ContentID == "" || doc.Assets[0].Size != 4 {
		t.Fatalf("asset = %+v", doc.Assets[0])
	}
	if paths := locationState(doc.Assets[0]); !same(paths, []string{"present old.mp3"}) {
		t.Fatalf("locations = %v", paths)
	}

	if err := os.Rename(filepath.Join(root, "old.mp3"), filepath.Join(root, "moved.mp3")); err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(ts.URL+"/rescan", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	if err := handler.waitForScan(); err != nil {
		t.Fatal(err)
	}
	doc = getInventory(t, ts.URL+"/inventory.json")
	if len(doc.Assets) != 1 || doc.Assets[0].ID != id {
		t.Fatalf("rename changed the asset: %+v", doc.Assets)
	}
	if paths := locationState(doc.Assets[0]); !same(paths, []string{"present moved.mp3"}) {
		t.Fatalf("locations after rename = %v", paths)
	}
}

func TestFailedRescanLeavesInventory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	res, err := http.Post(ts.URL+"/rescan", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	if err := handler.waitForScan(); err == nil {
		t.Fatal("expected scan to fail")
	}

	doc := getInventory(t, ts.URL+"/inventory.json")
	if len(doc.Assets) != 1 || !same(locationState(doc.Assets[0]), []string{"present song.mp3"}) {
		t.Fatalf("inventory changed: %+v", doc.Assets)
	}
}

func openStore(t *testing.T) *inventory.Store {
	t.Helper()
	store, err := inventory.Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func serve(t *testing.T, root string, store *inventory.Store) (*httptest.Server, *Handler) {
	t.Helper()
	handler := New(root, store)
	ts := httptest.NewServer(handler.Routes())
	t.Cleanup(ts.Close)
	return ts, handler
}

func getInventory(t *testing.T, url string) inventoryDocument {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return decodeInventory(t, res, http.StatusOK)
}

func postInventory(t *testing.T, url string, status int) inventoryDocument {
	t.Helper()
	res, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return decodeInventory(t, res, status)
}

func decodeInventory(t *testing.T, res *http.Response, status int) inventoryDocument {
	t.Helper()
	if res.StatusCode != status {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status %s: %s", res.Status, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %s", ct)
	}
	var doc inventoryDocument
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestSnapshotWhileScanRuns(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	handler := New(root, store)
	started := make(chan struct{})
	release := make(chan struct{})
	handler.scan = func(string, *inventory.Store) error {
		close(started)
		<-release
		return nil
	}
	ts := httptest.NewServer(handler.Routes())
	t.Cleanup(ts.Close)

	if !handler.trigger() {
		t.Fatal("scan did not start")
	}
	<-started
	if handler.trigger() {
		t.Fatal("second scan started while the first was running")
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/snapshot.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-Id", "ada")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status %s: %s", res.Status, body)
	}
	close(release)
	if err := handler.waitForScan(); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadSongByID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Album")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("song-bytes")
	if err := os.WriteFile(filepath.Join(dir, "track.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "copy.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, _ := serve(t, root, store)
	doc := getInventory(t, ts.URL+"/inventory.json")
	if len(doc.Assets) != 1 {
		t.Fatalf("assets = %d", len(doc.Assets))
	}
	id := doc.Assets[0].ID

	res, err := http.Get(ts.URL + "/songs/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, got)
	}
	if string(got) != string(body) {
		t.Fatalf("body = %q", got)
	}
	if ct := res.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content type %s", ct)
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "track.mp3") || strings.Contains(cd, "Album") {
		t.Fatalf("disposition = %s", cd)
	}
	if strings.Contains(res.Header.Get("Content-Type"), root) {
		t.Fatal("response exposes the library path")
	}

	missing, err := http.Get(ts.URL + "/songs/no-such-song")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status %s", missing.Status)
	}
	var missingDoc errorDocument
	if err := json.NewDecoder(missing.Body).Decode(&missingDoc); err != nil {
		t.Fatal(err)
	}
	if missingDoc.Error != "song not found" || strings.Contains(missingDoc.Error, root) {
		t.Fatalf("missing error = %q", missingDoc.Error)
	}

	if err := os.Remove(filepath.Join(dir, "track.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "copy.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	gone, err := http.Get(ts.URL + "/songs/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("gone status %s", gone.Status)
	}
	var goneDoc errorDocument
	if err := json.NewDecoder(gone.Body).Decode(&goneDoc); err != nil {
		t.Fatal(err)
	}
	if goneDoc.Error != "song unavailable" {
		t.Fatalf("gone error = %q", goneDoc.Error)
	}
}

func TestSnapshotIsTheDeviceBootstrap(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, _ := serve(t, root, store)

	res, err := http.Get(ts.URL + "/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %s", res.Status)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/snapshot.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-Id", "ada")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, body)
	}
	var cat inventory.Catalog
	if err := json.Unmarshal(body, &cat); err != nil {
		t.Fatal(err)
	}
	if cat.LibraryID == "" || cat.Seq != 1 || len(cat.Songs) != 1 {
		t.Fatalf("catalog = %+v", cat)
	}
	song := cat.Songs[0]
	if song.ID == "" || song.Hash == "" || !song.Available {
		t.Fatalf("song = %+v", song)
	}
	if string(song.Metadata) != "{}" || string(song.Analytics) != "{}" {
		t.Fatalf("documents = %s %s", song.Metadata, song.Analytics)
	}
	if strings.Contains(string(body), "\"path\"") {
		t.Fatalf("snapshot includes a path: %s", body)
	}
}

func TestArtworkByHash(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	body := webID3(webAPIC(picture))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, _ := serve(t, root, store)
	sum := sha256.Sum256(picture)
	hash := hex.EncodeToString(sum[:])

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/snapshot.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-Id", "ada")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, raw)
	}
	if !strings.Contains(string(raw), `"artwork": "`+hash+`"`) {
		t.Fatalf("snapshot = %s", raw)
	}
	if strings.Contains(string(raw), "\"path\"") || bytes.Contains(raw, picture) {
		t.Fatalf("snapshot includes a path or the image: %s", raw)
	}

	res, err = http.Get(ts.URL + "/artwork/" + hash)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, got)
	}
	if res.Header.Get("Content-Type") != "image/jpeg" || res.Header.Get("ETag") != `"`+hash+`"` {
		t.Fatalf("type %s etag %s", res.Header.Get("Content-Type"), res.Header.Get("ETag"))
	}
	if !bytes.Equal(got, picture) {
		t.Fatalf("body = %x", got)
	}
	if strings.Contains(res.Header.Get("Content-Type"), root) || bytes.Contains(got, []byte(root)) {
		t.Fatal("response exposes the library path")
	}

	cond, err := http.NewRequest(http.MethodGet, ts.URL+"/artwork/"+hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	cond.Header.Set("If-None-Match", `"`+hash+`"`)
	res, err = http.DefaultClient.Do(cond)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotModified {
		t.Fatalf("status %s", res.Status)
	}

	missing, err := http.Get(ts.URL + "/artwork/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("status %s", missing.Status)
	}
	var doc errorDocument
	if err := json.NewDecoder(missing.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Error != "artwork not found" || strings.Contains(doc.Error, root) {
		t.Fatalf("error = %q", doc.Error)
	}
}

func webAPIC(data []byte) []byte {
	payload := append([]byte("image/jpeg"), 0, 3, 0)
	payload = append([]byte{0}, append(payload, data...)...)
	frame := make([]byte, 10+len(payload))
	copy(frame[0:4], "APIC")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[10:], payload)
	return frame
}

func webID3(frames ...[]byte) []byte {
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

func locationState(asset inventory.Asset) []string {
	out := make([]string, 0, len(asset.Locations))
	for _, loc := range asset.Locations {
		state := "missing"
		if loc.Present {
			state = "present"
		}
		out = append(out, state+" "+loc.Path)
	}
	return out
}

func same(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestActionLogUsesTheHeaderUser(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, _ := serve(t, root, store)
	cat := getSnapshot(t, ts.URL, "ada")
	body := []byte(`{"actionId":"play-1","userId":"bea","songId":"` + cat.Songs[0].ID + `","type":"play","payload":{"userId":"bea","listenedMs":40}}`)
	res := postJSON(t, ts.URL+"/actions", "ada", "phone", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	var ack struct {
		Seq int64 `json:"seq"`
	}
	if err := json.NewDecoder(res.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	again := postJSON(t, ts.URL+"/actions", "ada", "phone", body)
	var ack2 struct {
		Seq int64 `json:"seq"`
	}
	if err := json.NewDecoder(again.Body).Decode(&ack2); err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusOK || ack2.Seq != ack.Seq {
		t.Fatalf("retry status %s seq %d want %d", again.Status, ack2.Seq, ack.Seq)
	}
	changed := bytes.Replace(body, []byte(`"listenedMs":40`), []byte(`"listenedMs":1`), 1)
	conflict := postJSON(t, ts.URL+"/actions", "ada", "phone", changed)
	conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status %s", conflict.Status)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/actions?since=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-Id", "ada")
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	var page struct {
		Actions []inventory.Action `json:"actions"`
	}
	if err := json.NewDecoder(got.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	var play inventory.Action
	for _, action := range page.Actions {
		if action.Type == "play" {
			play = action
		}
	}
	if play.UserID != "ada" || play.DeviceID != "phone" || play.Seq != ack.Seq {
		t.Fatalf("play = %+v", play)
	}
	bea, err := store.Bootstrap("bea")
	if err != nil {
		t.Fatal(err)
	}
	if string(bea.Songs[0].Analytics) != "{}" {
		t.Fatalf("bea analytics = %s", bea.Songs[0].Analytics)
	}
	ahead, err := http.NewRequest(http.MethodGet, ts.URL+"/actions?since=999999", nil)
	if err != nil {
		t.Fatal(err)
	}
	ahead.Header.Set("X-User-Id", "ada")
	res, err = http.DefaultClient.Do(ahead)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("ahead status %s", res.Status)
	}
}

func getSnapshot(t *testing.T, base, user string) inventory.Catalog {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/snapshot.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-Id", user)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var cat inventory.Catalog
	if err := json.NewDecoder(res.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	return cat
}

func postJSON(t *testing.T, url, user, device string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-Id", user)
	req.Header.Set("X-Device-Id", device)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
