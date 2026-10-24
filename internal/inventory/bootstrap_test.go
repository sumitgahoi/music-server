package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestBootstrapListsEachSongOnce(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "keep", ContentID: "alpha", Size: 2},
		Observation{Path: "drop", ContentID: "alpha", Size: 2},
		Observation{Path: "other", ContentID: "beta", Size: 4},
	)
	apply(t, s, "/music",
		Observation{Path: "keep", ContentID: "alpha", Size: 2},
		Observation{Path: "other", ContentID: "beta", Size: 4},
	)
	cat := mustBootstrap(t, s, "ada")
	if cat.LibraryID == "" || cat.Seq == 0 {
		t.Fatalf("catalog = %+v", cat)
	}
	if len(cat.Songs) != 2 {
		t.Fatalf("songs = %d, want 2", len(cat.Songs))
	}
	alpha := songByHash(t, cat, "alpha")
	beta := songByHash(t, cat, "beta")
	if !alpha.Available || !beta.Available {
		t.Fatalf("available alpha=%v beta=%v", alpha.Available, beta.Available)
	}
	if !bytes.Equal(alpha.Metadata, []byte(`{}`)) || !bytes.Equal(alpha.Analytics, []byte(`{}`)) {
		t.Fatalf("empty documents = %s %s", alpha.Metadata, alpha.Analytics)
	}
	again := mustBootstrap(t, s, "ada")
	if again.LibraryID != cat.LibraryID || again.Seq != cat.Seq {
		t.Fatal("library id changed")
	}
}

func TestBootstrapMissingSongStaysUnavailable(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "only", ContentID: "alpha", Size: 1})
	apply(t, s, "/music")
	cat := mustBootstrap(t, s, "ada")
	if len(cat.Songs) != 1 || cat.Songs[0].Available {
		t.Fatalf("songs = %+v", cat.Songs)
	}
}

func TestBootstrapAnalyticsFollowTheUser(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "song", ContentID: "alpha", Size: 3})
	songID := mustSnapshot(t, s).Assets[0].ID
	_, err := s.db.Exec(`
		INSERT INTO actions (action_id, song_id, action_type, received_at_ms, payload)
		VALUES ('scan-1', ?, 'song_upsert', 10, '{}')`, songID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`
		INSERT INTO analytics (user_id, song_id, play_count, finished_count, skip_count, listened_ms)
		VALUES ('ada', ?, 2, 1, 0, 5000)`, songID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO liked (user_id, song_id) VALUES ('ada', ?)`, songID); err != nil {
		t.Fatal(err)
	}
	ada := mustBootstrap(t, s, "ada")
	if ada.Seq != 2 {
		t.Fatalf("seq = %d", ada.Seq)
	}
	var stats userAnalytics
	if err := json.Unmarshal(songByHash(t, ada, "alpha").Analytics, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.PlayCount != 2 || len(ada.Liked) != 1 || ada.Liked[0] != songID {
		t.Fatalf("analytics = %+v liked %v", stats, ada.Liked)
	}
	bea := mustBootstrap(t, s, "bea")
	if bea.Seq != ada.Seq || len(bea.Liked) != 0 || !bytes.Equal(songByHash(t, bea, "alpha").Analytics, []byte(`{}`)) {
		t.Fatalf("bea analytics = %s", songByHash(t, bea, "alpha").Analytics)
	}
}

func TestBootstrapRejectsBlankUser(t *testing.T) {
	s := newStore(t)
	_, err := s.Bootstrap("  ")
	if !errors.Is(err, ErrUserID) {
		t.Fatalf("err = %v", err)
	}
}

func mustBootstrap(t *testing.T, s *Store, userID string) Catalog {
	t.Helper()
	cat, err := s.Bootstrap(userID)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func songByHash(t *testing.T, cat Catalog, hash string) CatalogSong {
	t.Helper()
	for _, song := range cat.Songs {
		if song.Hash == hash {
			return song
		}
	}
	t.Fatalf("missing hash %s", hash)
	return CatalogSong{}
}
