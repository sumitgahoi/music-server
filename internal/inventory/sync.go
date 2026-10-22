package inventory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"time"
)

// ErrConflict means this action id was already accepted with a different body.
var ErrConflict = errors.New("action conflict")

// ErrCursor means the client's cursor is past the greatest action seq.
var ErrCursor = errors.New("cursor is ahead of the server")

// ErrAction means the action cannot be accepted.
var ErrAction = errors.New("invalid action")

// Submit is one user action. The user and device come from the caller,
// which is the authenticated request, not from this body.
type Submit struct {
	ActionID     string
	SongID       string
	Type         string
	OccurredAtMs *int64
	Payload      json.RawMessage
}

// Action is one log row a device applies.
type Action struct {
	Seq          int64           `json:"seq"`
	ID           string          `json:"actionId"`
	UserID       string          `json:"userId,omitempty"`
	DeviceID     string          `json:"deviceId,omitempty"`
	SongID       string          `json:"songId"`
	Type         string          `json:"type"`
	OccurredAtMs *int64          `json:"occurredAtMs,omitempty"`
	ReceivedAtMs int64           `json:"receivedAtMs"`
	Payload      json.RawMessage `json:"payload"`
}

type deviceView struct {
	hash      string
	metadata  string
	artwork   string
	available bool
}

func (s *Store) nowMS() int64 {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UnixMilli()
}

// Accept appends one play, like, or unlike and updates that user's analytics.
// The same action id and body returns the original seq and changes nothing.
func (s *Store) Accept(userID, deviceID string, in Submit) (int64, error) {
	if err := validateUserID(userID); err != nil {
		return 0, err
	}
	if err := validateActionID(in.ActionID); err != nil || in.SongID == "" {
		return 0, ErrAction
	}
	payload, listened, err := canonicalUserPayload(in.Type, in.Payload)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var present int
	err = tx.QueryRow(`SELECT 1 FROM assets WHERE id = ?`, in.SongID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSongNotFound
	}
	if err != nil {
		return 0, err
	}

	var existingSeq int64
	var existingType, existingSong string
	var existingPayload []byte
	err = tx.QueryRow(`SELECT seq, action_type, song_id, payload FROM actions WHERE action_id = ?`, in.ActionID).
		Scan(&existingSeq, &existingType, &existingSong, &existingPayload)
	if err == nil {
		previous, _, perr := canonicalUserPayload(existingType, existingPayload)
		if perr != nil || existingType != in.Type || existingSong != in.SongID || !bytes.Equal(previous, payload) {
			return 0, ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return existingSeq, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	var device sql.NullString
	if deviceID != "" {
		device = sql.NullString{String: deviceID, Valid: true}
	}
	var occurred sql.NullInt64
	if in.OccurredAtMs != nil {
		occurred = sql.NullInt64{Int64: *in.OccurredAtMs, Valid: true}
	}
	res, err := tx.Exec(`
		INSERT INTO actions (action_id, user_id, device_id, song_id, action_type, occurred_at_ms, received_at_ms, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ActionID, userID, device, in.SongID, in.Type, occurred, s.nowMS(), payload)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := reduceUserAction(tx, userID, in.SongID, in.Type, listened, in.OccurredAtMs); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// ActionsAfter returns catalog actions and this user's actions with a greater seq.
// A cursor past the log is ErrCursor so the caller bootstraps again.
func (s *Store) ActionsAfter(userID string, seq int64) ([]Action, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if seq < 0 {
		return nil, ErrAction
	}
	var max int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM actions`).Scan(&max); err != nil {
		return nil, err
	}
	if seq > max {
		return nil, ErrCursor
	}
	rows, err := s.db.Query(`
		SELECT seq, action_id, user_id, device_id, song_id, action_type, occurred_at_ms, received_at_ms, payload
		FROM actions
		WHERE seq > ? AND (user_id IS NULL OR user_id = ?)
		ORDER BY seq`, seq, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Action{}
	for rows.Next() {
		var action Action
		var user, device sql.NullString
		var occurred sql.NullInt64
		var payload []byte
		if err := rows.Scan(&action.Seq, &action.ID, &user, &device, &action.SongID, &action.Type, &occurred, &action.ReceivedAtMs, &payload); err != nil {
			return nil, err
		}
		if user.Valid {
			action.UserID = user.String
		}
		if device.Valid {
			action.DeviceID = device.String
		}
		if occurred.Valid {
			v := occurred.Int64
			action.OccurredAtMs = &v
		}
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}
		action.Payload = json.RawMessage(payload)
		out = append(out, action)
	}
	return out, rows.Err()
}

func canonicalUserPayload(typ string, raw json.RawMessage) ([]byte, int64, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = []byte(`{}`)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, 0, ErrAction
	}
	delete(obj, "userId")
	switch typ {
	case "like", "unlike":
		if len(obj) != 0 {
			return nil, 0, ErrAction
		}
		return []byte(`{}`), 0, nil
	case "play":
		if len(obj) == 0 {
			return []byte(`{}`), 0, nil
		}
		rawMS, ok := obj["listenedMs"]
		if !ok || len(obj) != 1 {
			return nil, 0, ErrAction
		}
		var n int64
		if err := json.Unmarshal(rawMS, &n); err != nil || n < 0 {
			return nil, 0, ErrAction
		}
		body, err := json.Marshal(struct {
			ListenedMs int64 `json:"listenedMs"`
		}{n})
		if err != nil {
			return nil, 0, err
		}
		return body, n, nil
	default:
		return nil, 0, ErrAction
	}
}

func reduceUserAction(tx *sql.Tx, userID, songID, typ string, listened int64, occurred *int64) error {
	switch typ {
	case "play":
		var at any
		if occurred != nil {
			at = *occurred
		}
		_, err := tx.Exec(`
			INSERT INTO analytics (user_id, song_id, play_count, listened_ms, last_played_at_ms)
			VALUES (?, ?, 1, ?, ?)
			ON CONFLICT (user_id, song_id) DO UPDATE SET
				play_count = play_count + 1,
				listened_ms = listened_ms + excluded.listened_ms,
				last_played_at_ms = COALESCE(excluded.last_played_at_ms, analytics.last_played_at_ms)`,
			userID, songID, listened, at)
		return err
	case "like", "unlike":
		liked := 0
		if typ == "like" {
			liked = 1
		}
		_, err := tx.Exec(`
			INSERT INTO analytics (user_id, song_id, liked)
			VALUES (?, ?, ?)
			ON CONFLICT (user_id, song_id) DO UPDATE SET liked = excluded.liked`,
			userID, songID, liked)
		return err
	default:
		return ErrAction
	}
}

func validateActionID(id string) error {
	if id == "" || len(id) > 128 {
		return ErrAction
	}
	for _, r := range id {
		if r <= ' ' || r > '~' {
			return ErrAction
		}
	}
	return nil
}

func loadDeviceViews(tx *sql.Tx) (map[string]deviceView, error) {
	rows, err := tx.Query(`
		SELECT a.id, a.content_id, a.metadata, a.artwork_hash, l.path, l.present
		FROM assets a
		LEFT JOIN locations l ON l.asset_id = a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		view    deviceView
		visible bool
	}
	byID := map[string]*acc{}
	for rows.Next() {
		var id, hash, metadata string
		var artwork, filePath sql.NullString
		var present sql.NullInt64
		if err := rows.Scan(&id, &hash, &metadata, &artwork, &filePath, &present); err != nil {
			return nil, err
		}
		row := byID[id]
		if row == nil {
			row = &acc{view: deviceView{
				hash:     hash,
				metadata: normalizeMetadata(metadata),
			}}
			if artwork.Valid {
				row.view.artwork = artwork.String
			}
			byID[id] = row
		}
		if filePath.Valid && songPath(filePath.String) {
			row.visible = true
			if present.Valid && present.Int64 == 1 {
				row.view.available = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]deviceView, len(byID))
	for id, row := range byID {
		if row.visible {
			out[id] = row.view
		}
	}
	return out, nil
}

func songPath(p string) bool {
	return supportedAudio(p) || path.Ext(p) == ""
}

func logDeviceChanges(tx *sql.Tx, before, after map[string]deviceView, received int64) error {
	ids := make([]string, 0, len(after))
	seen := map[string]struct{}{}
	for id := range after {
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range before {
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		next, ok := after[id]
		if !ok {
			continue
		}
		prev, had := before[id]
		if had && prev == next {
			continue
		}
		if had && prev.available && !next.available && prev.hash == next.hash && prev.metadata == next.metadata && prev.artwork == next.artwork {
			if err := appendAction(tx, id, "song_unavailable", received, []byte(`{"available":false}`)); err != nil {
				return err
			}
			continue
		}
		if err := appendSongView(tx, id, next, received); err != nil {
			return err
		}
	}
	return nil
}

func appendSongUpsert(tx *sql.Tx, songID string, received int64) error {
	views, err := loadDeviceViews(tx)
	if err != nil {
		return err
	}
	view, ok := views[songID]
	if !ok {
		return nil
	}
	return appendSongView(tx, songID, view, received)
}

func appendSongView(tx *sql.Tx, songID string, view deviceView, received int64) error {
	payload, err := json.Marshal(songUpsertBody{
		Hash:      view.hash,
		Available: view.available,
		Metadata:  json.RawMessage(view.metadata),
		Artwork:   view.artwork,
	})
	if err != nil {
		return err
	}
	return appendAction(tx, songID, "song_upsert", received, payload)
}

type songUpsertBody struct {
	Hash      string          `json:"hash"`
	Available bool            `json:"available"`
	Metadata  json.RawMessage `json:"metadata"`
	Artwork   string          `json:"artwork,omitempty"`
}

func appendAction(tx *sql.Tx, songID, typ string, received int64, payload []byte) error {
	id, err := newAssetID()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		INSERT INTO actions (action_id, song_id, action_type, received_at_ms, payload)
		VALUES (?, ?, ?, ?, ?)`, id, songID, typ, received, payload)
	return err
}
