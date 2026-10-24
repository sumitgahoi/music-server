package inventory

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// ErrUserID means the bootstrap was asked for without a user.
var ErrUserID = errors.New("user id is required")

// Catalog is the device bootstrap: every song and this user's analytics
// at one action sequence.
type Catalog struct {
	LibraryID string        `json:"libraryId"`
	Seq       int64         `json:"seq"`
	Songs     []CatalogSong `json:"songs"`
	// Liked is this user's liked song ids, most recently liked first.
	Liked []string `json:"liked"`
}

// CatalogSong is one song as a device stores it. Paths are not included.
type CatalogSong struct {
	ID        string          `json:"id"`
	Hash      string          `json:"hash"`
	Available bool            `json:"available"`
	Metadata  json.RawMessage `json:"metadata"`
	Analytics json.RawMessage `json:"analytics"`
	Artwork   string          `json:"artwork,omitempty"`
}

type userAnalytics struct {
	PlayCount      int    `json:"playCount"`
	FinishedCount  int    `json:"finishedCount"`
	SkipCount      int    `json:"skipCount"`
	ListenedMs     int64  `json:"listenedMs"`
	LastPlayedAtMs *int64 `json:"lastPlayedAtMs"`
}

// Bootstrap reads the catalog and one user's analytics in a single
// transaction. The first read fixes the SQLite snapshot, and seq is the
// greatest action visible in that snapshot.
func (s *Store) Bootstrap(userID string) (Catalog, error) {
	if err := validateUserID(userID); err != nil {
		return Catalog{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Catalog{}, err
	}
	defer tx.Rollback()

	var cat Catalog
	if err := tx.QueryRow(`SELECT library_id FROM library_state WHERE singleton_id = 1`).Scan(&cat.LibraryID); err != nil {
		return Catalog{}, err
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM actions`).Scan(&cat.Seq); err != nil {
		return Catalog{}, err
	}
	if s.afterSnapshot != nil {
		s.afterSnapshot()
	}
	songs, err := loadCatalogSongs(tx)
	if err != nil {
		return Catalog{}, err
	}
	if err := attachAnalytics(tx, userID, songs); err != nil {
		return Catalog{}, err
	}
	cat.Songs = songs
	if cat.Liked, err = loadLiked(tx, userID); err != nil {
		return Catalog{}, err
	}
	if err := tx.Commit(); err != nil {
		return Catalog{}, err
	}
	return cat, nil
}

func loadCatalogSongs(tx *sql.Tx) ([]CatalogSong, error) {
	rows, err := tx.Query(`
		SELECT a.id, a.content_id, a.metadata, a.artwork_hash,
			EXISTS(SELECT 1 FROM locations l WHERE l.asset_id = a.id AND l.present = 1)
		FROM assets a
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	songs := []CatalogSong{}
	for rows.Next() {
		var song CatalogSong
		var metadata string
		var artwork sql.NullString
		var available int
		if err := rows.Scan(&song.ID, &song.Hash, &metadata, &artwork, &available); err != nil {
			return nil, err
		}
		song.Available = available == 1
		song.Metadata = json.RawMessage(normalizeMetadata(metadata))
		song.Analytics = json.RawMessage(`{}`)
		if artwork.Valid {
			song.Artwork = artwork.String
		}
		songs = append(songs, song)
	}
	return songs, rows.Err()
}

func attachAnalytics(tx *sql.Tx, userID string, songs []CatalogSong) error {
	rows, err := tx.Query(`
		SELECT song_id, play_count, finished_count, skip_count, listened_ms, last_played_at_ms
		FROM analytics
		WHERE user_id = ?`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	bySong := map[string]json.RawMessage{}
	for rows.Next() {
		var songID string
		var stats userAnalytics
		var lastPlayed sql.NullInt64
		if err := rows.Scan(&songID, &stats.PlayCount, &stats.FinishedCount, &stats.SkipCount, &stats.ListenedMs, &lastPlayed); err != nil {
			return err
		}
		if lastPlayed.Valid {
			v := lastPlayed.Int64
			stats.LastPlayedAtMs = &v
		}
		raw, err := json.Marshal(stats)
		if err != nil {
			return err
		}
		bySong[songID] = raw
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range songs {
		if raw, ok := bySong[songs[i].ID]; ok {
			songs[i].Analytics = raw
		}
	}
	return nil
}

func loadLiked(tx *sql.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(`SELECT song_id FROM liked WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	liked := []string{}
	for rows.Next() {
		var songID string
		if err := rows.Scan(&songID); err != nil {
			return nil, err
		}
		liked = append(liked, songID)
	}
	return liked, rows.Err()
}

func validateUserID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > 128 {
		return ErrUserID
	}
	for _, r := range id {
		if r <= ' ' || r > '~' {
			return ErrUserID
		}
	}
	return nil
}
