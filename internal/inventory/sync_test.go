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
	in := Submit{ActionID: "p1", SongID: songID, Type: "play", OccurredAtMs: &when, Payload: []byte(`{"userId":"bea","listenedMs":5}`)}
	seq, err := s.Accept("ada", "phone", in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Accept("ada", "phone", in)
	if err != nil || again != seq {
		t.Fatalf("retry seq %d err %v", again, err)
	}
	in.Payload = []byte(`{"listenedMs":9}`)
	if _, err := s.Accept("ada", "phone", in); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	if _, err := s.Accept("ada", "phone", Submit{ActionID: "done", SongID: songID, Type: "finish"}); !errors.Is(err, ErrAction) {
		t.Fatalf("finish err = %v", err)
	}
	ada := mustBootstrap(t, s, "ada")
	bea := mustBootstrap(t, s, "bea")
	var stats userAnalytics
	if err := json.Unmarshal(songByHash(t, ada, "alpha").Analytics, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.PlayCount != 1 || stats.ListenedMs != 5 || stats.LastPlayedAtMs == nil || *stats.LastPlayedAtMs != 10 {
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
	if stats.PlayCount != 1 || stats.Liked == nil || *stats.Liked != 0 {
		t.Fatalf("folded = %+v", stats)
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
	if _, err := s.Accept("ada", "phone", Submit{ActionID: "p1", SongID: songID, Type: "play", Payload: []byte(`{}`)}); err != nil {
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
