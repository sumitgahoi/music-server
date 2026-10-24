package inventory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestScanLogsTheDeviceView(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.mp3"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	first, err := s.ActionsAfter("ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Type != "song_upsert" || first[0].UserID != "" {
		t.Fatalf("actions = %+v", first)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a.mp3"), filepath.Join(root, "b.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	afterMove, err := s.ActionsAfter("ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterMove) != 1 {
		t.Fatalf("move logged %d actions", len(afterMove))
	}
	if err := os.Remove(filepath.Join(root, "b.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	gone, err := s.ActionsAfter("ada", first[0].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].Type != "song_unavailable" || string(gone[0].Payload) != `{"available":false}` {
		t.Fatalf("gone = %+v", gone)
	}
}

func TestMetadataChangeIsASongUpsert(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "song.mp3", ContentID: "alpha", Size: 3, Metadata: `{}`})
	apply(t, s, "/music", Observation{Path: "song.mp3", ContentID: "alpha", Size: 3, Metadata: `{"title":"A"}`})
	actions, err := s.ActionsAfter("ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[1].Type != "song_upsert" || !bytes.Contains(actions[1].Payload, []byte(`"title":"A"`)) {
		t.Fatalf("actions = %+v", actions)
	}
}

func TestAcceptIsIdempotentForOneUser(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "song.mp3", ContentID: "alpha", Size: 3})
	songID := mustSnapshot(t, s).Assets[0].ID
	when := int64(10)
	in := Submit{ActionID: "p1", SongID: songID, Type: "play", OccurredAtMs: &when, Payload: []byte(`{"userId":"bea","listenedMs":30001}`)}
	seq, err := s.Accept("ada", "phone", in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Accept("ada", "phone", in)
	if err != nil || again != seq {
		t.Fatalf("retry seq %d err %v", again, err)
	}
	in.Payload = []byte(`{"listenedMs":40000}`)
	if _, err := s.Accept("ada", "phone", in); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	if _, err := s.Accept("ada", "phone", Submit{ActionID: "short", SongID: songID, Type: "play", Payload: []byte(`{"listenedMs":30000}`)}); !errors.Is(err, ErrAction) {
		t.Fatalf("short play err = %v", err)
	}
	ada := mustBootstrap(t, s, "ada")
	bea := mustBootstrap(t, s, "bea")
	var stats userAnalytics
	if err := json.Unmarshal(songByHash(t, ada, "alpha").Analytics, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.PlayCount != 1 || stats.ListenedMs != 30001 || stats.LastPlayedAtMs == nil || *stats.LastPlayedAtMs != 10 {
		t.Fatalf("ada = %+v", stats)
	}
	if string(songByHash(t, bea, "alpha").Analytics) != "{}" {
		t.Fatalf("bea = %s", songByHash(t, bea, "alpha").Analytics)
	}
	like, err := s.Accept("ada", "phone", Submit{ActionID: "l1", SongID: songID, Type: "like"})
	if err != nil {
		t.Fatal(err)
	}
	unlike, err := s.Accept("ada", "other", Submit{ActionID: "l2", SongID: songID, Type: "unlike"})
	if err != nil || unlike <= like {
		t.Fatalf("unlike seq %d like %d err %v", unlike, like, err)
	}
	ada = mustBootstrap(t, s, "ada")
	if err := json.Unmarshal(songByHash(t, ada, "alpha").Analytics, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.PlayCount != 1 || len(ada.Liked) != 0 {
		t.Fatalf("folded = %+v liked %v", stats, ada.Liked)
	}
}

func TestLikedListsNewestFirstAndKeepsTheFirstLike(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "a", ContentID: "alpha", Size: 1},
		Observation{Path: "b", ContentID: "beta", Size: 1},
		Observation{Path: "c", ContentID: "gamma", Size: 1},
	)
	cat := mustBootstrap(t, s, "ada")
	alpha := songByHash(t, cat, "alpha").ID
	beta := songByHash(t, cat, "beta").ID
	gamma := songByHash(t, cat, "gamma").ID
	accept := func(id, song, typ string) {
		t.Helper()
		if _, err := s.Accept("ada", "phone", Submit{ActionID: id, SongID: song, Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	accept("l1", alpha, "like")
	accept("l2", beta, "like")
	accept("l3", gamma, "like")
	accept("l4", alpha, "like")
	assertLiked(t, mustBootstrap(t, s, "ada").Liked, gamma, beta, alpha)

	accept("u1", alpha, "unlike")
	accept("l5", alpha, "like")
	accept("u2", beta, "unlike")
	assertLiked(t, mustBootstrap(t, s, "ada").Liked, alpha, gamma)
	assertLiked(t, mustBootstrap(t, s, "bea").Liked)
}

func assertLiked(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("liked = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("liked = %v, want %v", got, want)
		}
	}
}

func TestServerRestartKeepsAcceptedState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply("/music", []Observation{{Path: "song.mp3", ContentID: "alpha", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	songID := mustSnapshot(t, s).Assets[0].ID
	if _, err := s.Accept("ada", "phone", Submit{ActionID: "p1", SongID: songID, Type: "play", Payload: []byte(`{"listenedMs":30001}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	actions, err := s.ActionsAfter("ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	var plays int
	for _, action := range actions {
		if action.Type == "play" {
			plays++
		}
	}
	if plays != 1 {
		t.Fatalf("plays = %d", plays)
	}
	cat := mustBootstrap(t, s, "ada")
	var stats userAnalytics
	if err := json.Unmarshal(cat.Songs[0].Analytics, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.PlayCount != 1 {
		t.Fatalf("play count = %d", stats.PlayCount)
	}
}

func TestListeningCountsMatchThePublishedActions(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "song.mp3", ContentID: "alpha", Size: 3})
	songID := mustSnapshot(t, s).Assets[0].ID
	accept := func(id, typ, payload string) {
		t.Helper()
		var raw []byte
		if payload != "" {
			raw = []byte(payload)
		}
		if _, err := s.Accept("ada", "phone", Submit{ActionID: id, SongID: songID, Type: typ, Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	accept("skip-early", "skip", "")
	accept("finish-short", "finish", "")
	accept("play-1", "play", `{"listenedMs":30001}`)
	accept("skip-late", "skip", "")
	accept("finish-long", "finish", "")
	if _, err := s.Accept("ada", "phone", Submit{ActionID: "save-1", SongID: songID, Type: "save"}); !errors.Is(err, ErrAction) {
		t.Fatalf("save err = %v", err)
	}
	if _, err := s.Accept("ada", "phone", Submit{ActionID: "add-1", SongID: songID, Type: "playlist_add"}); !errors.Is(err, ErrAction) {
		t.Fatalf("playlist add err = %v", err)
	}
	stats := analyticsFor(t, s, "ada", "alpha")
	if stats.PlayCount != 1 || stats.ListenedMs != 30001 || stats.SkipCount != 2 || stats.FinishedCount != 2 {
		t.Fatalf("counts = %+v", stats)
	}
}

func analyticsFor(t *testing.T, s *Store, user, hash string) userAnalytics {
	t.Helper()
	cat := mustBootstrap(t, s, user)
	var stats userAnalytics
	if err := json.Unmarshal(songByHash(t, cat, hash).Analytics, &stats); err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestBootstrapMissesAConcurrentWrite(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "early.mp3", ContentID: "early", Size: 1})
	s.afterSnapshot = func() {
		db, err := sql.Open("sqlite", s.path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO assets (id, content_id, size, metadata) VALUES ('late', 'late', 1, '{}')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO locations (asset_id, path, present) VALUES ('late', 'late.mp3', 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO actions (action_id, song_id, action_type, received_at_ms, payload) VALUES ('late-1', 'late', 'song_upsert', 1, '{"hash":"late","available":true,"metadata":{}}')`); err != nil {
			t.Fatal(err)
		}
	}
	cat := mustBootstrap(t, s, "ada")
	s.afterSnapshot = nil
	for _, song := range cat.Songs {
		if song.ID == "late" {
			t.Fatal("bootstrap saw the concurrent song")
		}
	}
	later, err := s.ActionsAfter("ada", cat.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0].SongID != "late" || later[0].Type != "song_upsert" {
		t.Fatalf("catch-up = %+v", later)
	}
}
