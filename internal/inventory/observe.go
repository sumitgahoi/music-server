package inventory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Observation is one regular file seen inside the music folder.
// ContentID identifies the file's exact bytes. The scanner uses a
// SHA-256 hex string; tests may use a shorter stand-in.
type Observation struct {
	Path         string
	ContentID    string
	Size         int64
	Metadata     string
	Mtime        int64
	WriteMtime   bool
	ArtworkHash  string
	ArtworkMIME  string
	ArtworkKnown bool
	StoreChecked bool
}

// knownFile is a present path whose bytes were read on an earlier scan.
type knownFile struct {
	Size         int64
	Mtime        int64
	ContentID    string
	Metadata     string
	ArtworkHash  string
	ArtworkMIME  string
	ArtworkKnown bool
	StoreChecked bool
}

// CleanRoot returns the absolute music folder, with links resolved.
func CleanRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("music folder is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a folder", resolved)
	}
	return resolved, nil
}

// Scan reads root and stores the inventory.
// A failure while reading leaves the stored inventory unchanged.
func Scan(root string, store *Store) error {
	return scan(root, store, store.skipDB, func(name string) (io.ReadCloser, error) {
		return os.Open(name)
	}, newStoreCatalog(nil))
}

func scan(root string, store *Store, skip func(string) bool, open func(string) (io.ReadCloser, error), covers storeCovers) error {
	root, err := CleanRoot(root)
	if err != nil {
		return err
	}
	known, err := store.knownPresent()
	if err != nil {
		return err
	}
	files, err := observe(root, skip, open, known, store.writeArtwork, covers)
	if err != nil {
		return err
	}
	return store.Apply(root, files)
}

func (s *Store) knownPresent() (map[string]knownFile, error) {
	rows, err := s.db.Query(`
		SELECT l.path, l.mtime_ns, a.content_id, a.size, a.metadata, a.artwork_hash, a.artwork_mime, a.store_checked
		FROM locations l
		JOIN assets a ON a.id = l.asset_id
		WHERE l.present = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]knownFile{}
	for rows.Next() {
		var path string
		var rec knownFile
		var artworkHash, artworkMIME sql.NullString
		var storeChecked int
		if err := rows.Scan(&path, &rec.Mtime, &rec.ContentID, &rec.Size, &rec.Metadata, &artworkHash, &artworkMIME, &storeChecked); err != nil {
			return nil, err
		}
		rec.Metadata = normalizeMetadata(rec.Metadata)
		rec.ArtworkKnown = artworkHash.Valid
		rec.ArtworkHash = artworkHash.String
		rec.ArtworkMIME = artworkMIME.String
		rec.StoreChecked = storeChecked == 1
		out[path] = rec
	}
	return out, rows.Err()
}

// sameBytes is true when size already matched and the file is not worth reading.
// A stored mtime must match. Before any mtime is stored, a file that already
// has metadata is treated as unchanged so the first fast scan only records time.
func sameBytes(prev knownFile, mtime int64) bool {
	if prev.Mtime != 0 {
		return prev.Mtime == mtime
	}
	return prev.Metadata != "{}"
}

func observe(root string, skip func(string) bool, open func(string) (io.ReadCloser, error), known map[string]knownFile, save func(*coverArt) error, covers storeCovers) ([]Observation, error) {
	var out []Observation
	err := filepath.WalkDir(root, func(full string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if skip != nil && skip(full) {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err := validatePath(rel); err != nil {
			return err
		}
		if !supportedAudio(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mtime := info.ModTime().UnixNano()
		if prev, ok := known[rel]; ok && prev.Size == info.Size() && sameBytes(prev, mtime) {
			obs := Observation{
				Path: rel, ContentID: prev.ContentID, Size: prev.Size,
				Metadata: prev.Metadata, Mtime: mtime,
				WriteMtime:   prev.Mtime == 0,
				ArtworkHash:  prev.ArtworkHash,
				ArtworkMIME:  prev.ArtworkMIME,
				ArtworkKnown: prev.ArtworkKnown,
				StoreChecked: prev.StoreChecked,
			}
			// The hash is stored once. A deleted cache file is not a reason
			// to open the audio again; the request path rebuilds that file.
			// A purchase with no embedded cover still owes one store lookup.
			if !prev.ArtworkKnown {
				art, knownArt, err := coverAt(open, full, rel)
				if err != nil {
					return err
				}
				if knownArt && art == nil && storeAudio(rel) {
					var checked bool
					art, checked, err = resolveStore(open, full, rel, info.Size(), covers)
					if err != nil {
						return err
					}
					obs.StoreChecked = checked
				} else if knownArt {
					obs.StoreChecked = true
				}
				if knownArt {
					if err := rememberArtwork(save, rel, art); err != nil {
						return err
					}
					obs.ArtworkHash, obs.ArtworkMIME, obs.ArtworkKnown = artworkFields(art, true)
				}
			} else if prev.ArtworkHash == "" && !prev.StoreChecked && storeAudio(rel) {
				art, checked, err := resolveStore(open, full, rel, info.Size(), covers)
				if err != nil {
					return err
				}
				if err := rememberArtwork(save, rel, art); err != nil {
					return err
				}
				obs.ArtworkHash, obs.ArtworkMIME, obs.ArtworkKnown = artworkFields(art, true)
				obs.StoreChecked = checked
			}
			out = append(out, obs)
			return nil
		}
		rc, err := open(full)
		if err != nil {
			return err
		}
		sum, n, readErr := hashReader(rc)
		if readErr != nil {
			rc.Close()
			return fmt.Errorf("read %s: %w", rel, readErr)
		}
		meta := "{}"
		var art *coverArt
		artworkKnown := false
		if rs, ok := rc.(io.ReadSeeker); ok {
			if _, err := rs.Seek(0, io.SeekStart); err != nil {
				rc.Close()
				return fmt.Errorf("rewind %s: %w", rel, err)
			}
			var tagErr error
			meta, art, tagErr = extractMetadata(rs, n)
			if tagErr != nil {
				log.Printf("metadata %s: %v", rel, tagErr)
				meta = "{}"
				art = nil
			}
			artworkKnown = true
		}
		if err := rc.Close(); err != nil {
			return fmt.Errorf("close %s: %w", rel, err)
		}
		storeChecked := false
		if artworkKnown && art == nil && storeAudio(rel) {
			var err error
			art, storeChecked, err = resolveStore(open, full, rel, n, covers)
			if err != nil {
				return err
			}
		} else if artworkKnown {
			storeChecked = true
		}
		if err := rememberArtwork(save, rel, art); err != nil {
			return err
		}
		hash, mime, knownArt := artworkFields(art, artworkKnown)
		out = append(out, Observation{
			Path: rel, ContentID: sum, Size: n, Metadata: meta,
			Mtime: mtime, WriteMtime: true,
			ArtworkHash: hash, ArtworkMIME: mime, ArtworkKnown: knownArt,
			StoreChecked: storeChecked,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// coverAt reads the front cover. A tag error leaves known false so the
// next scan tries again. An open failure fails the scan.
func coverAt(open func(string) (io.ReadCloser, error), full, rel string) (*coverArt, bool, error) {
	rc, err := open(full)
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()
	rs, ok := rc.(io.ReadSeeker)
	if !ok {
		return nil, false, nil
	}
	art, err := readCover(rs)
	if err != nil {
		log.Printf("artwork %s: %v", rel, err)
		return nil, false, nil
	}
	return art, true, nil
}

func rememberArtwork(save func(*coverArt) error, rel string, art *coverArt) error {
	if art == nil || save == nil {
		return nil
	}
	if err := save(art); err != nil {
		return fmt.Errorf("artwork %s: %w", rel, err)
	}
	return nil
}

func artworkFields(art *coverArt, known bool) (string, string, bool) {
	if art == nil {
		return "", "", known
	}
	return art.Hash, art.MIME, known
}

func hashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Extensions for the formats Apple lists for iPhone audio playback:
// AAC, MP3, Apple Lossless, FLAC, Dolby Digital, Dolby Digital Plus,
// and Dolby Atmos, plus WAV and AIFF, which that list leaves under
// "include" and which earlier iPhone specs name as Linear PCM.
var audioExt = map[string]struct{}{
	".aac":  {},
	".ac3":  {},
	".aif":  {},
	".aiff": {},
	".caf":  {},
	".eac3": {},
	".ec3":  {},
	".flac": {},
	".m4a":  {},
	".m4b":  {},
	".mp3":  {},
	".mp4":  {},
	".wav":  {},
	".wave": {},
}

func supportedAudio(p string) bool {
	_, ok := audioExt[strings.ToLower(path.Ext(p))]
	return ok
}

func validatePath(p string) error {
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.Contains(p, `\`) {
		return fmt.Errorf("invalid path %q", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("invalid path %q", p)
	}
	return nil
}
