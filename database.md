# Proposed database design

SQLite on the server and client. Flexible metadata is JSONB. Identity,
indexing, and sync cursors are real columns.

Validate JSONB in the actual Go and iOS SQLite builds. The API carries
ordinary JSON. JSONB stays inside SQLite.

## 1. inventory

One row per song. Rows are kept. file_hash is the song's current bytes.

- song_id TEXT PRIMARY KEY — stable UUID
- file_hash TEXT NOT NULL UNIQUE — SHA-256 of the complete file
- size_bytes INTEGER NOT NULL
- metadata BLOB NOT NULL — SQLite JSONB; `{}` until tags are read
- artwork_hash TEXT — SHA-256 of the front-cover bytes; empty when the scan found no cover; null until a scan has looked
- artwork_mime TEXT — content type of those bytes; null when there is no cover
- store_checked INTEGER NOT NULL — 1 when a store lookup is finished or the file has no store id; 0 when a file with no cover still owes that lookup
- available INTEGER NOT NULL — 1 when any location is present

Metadata comes from the current bytes. It excludes paths and the cover
image. artwork_hash sits beside it. The same hash is one image shared by
every song that carries those bytes. The bytes live in a cache directory
beside the database. A scan writes that file when it already has the audio
file open. An AAC file with no embedded cover and an iTunes cnID is
looked up then. Songs that share a plID share one download, at 600 pixels.
The image is hashed and cached like an embedded cover. A lookup that finds
nothing is recorded and not repeated. A network failure leaves store_checked
at 0 so the next scan tries again. A later scan does not reopen an unchanged
file to refill a deleted cache, and it does not call Apple again once the
lookup is finished. GET /artwork/{hash} serves the file and, when it is
missing, reads one present copy the same way a scan does: the embedded
picture, then the iTunes id. The downloaded bytes are served when they hash to the requested hash.
When Apple has replaced that image, the new bytes are stored under their
own hash and the song is pointed at it. The old URL is not found. The
next snapshot carries the new hash. A network failure is an error and
does not change the stored hash. An in-place hash update replaces metadata
and the cover when tags are read.

file_hash identifies exact bytes. Identical bytes are one song wherever
they sit, so a move or rename inside the root relinks by hash.

A byte change at a path this song already occupies updates file_hash
and keeps song_id when the previous bytes are gone from the library
and the new hash is not already a song.

The old row stays, its paths go absent, and history stays on the old id
in these MVP cases. A later milestone can relink them:

- The file moves and its bytes change before the next scan.
- The path is replaced by bytes that already belong to another song,
  and this song has no remaining copy.
- One copy is retagged while another copy of the old bytes remains.
- One scan turns several songs into the same new bytes, or retags
  several copies into different bytes.

available is written in the scan transaction. One missing copy leaves
the song available. A device is told only when availability, hash,
metadata, or artwork changes.

song_id is what clients, analytics, and actions store.

Example metadata:

{
  "title": "Femininomenon",
  "artists": ["Chappell Roan"],
  "album": "The Rise and Fall of a Midwest Princess",
  "albumArtist": "Chappell Roan",
  "compilation": false,
  "trackNumber": 1,
  "discNumber": 1,
  "year": 2023,
  "durationMs": 219000,
  "codec": "aac",
  "bitrate": 256000
}

## 2. file_location

Server-only. Many rows per song.

- location_id INTEGER PRIMARY KEY
- song_id TEXT NOT NULL REFERENCES inventory(song_id)
- path TEXT NOT NULL — relative to the library root
- mtime_ns INTEGER NOT NULL
- ctime_ns INTEGER — nullable where unavailable or unreliable
- present INTEGER NOT NULL

UNIQUE (song_id, path). At most one present row for a path.

The database stores the library root and refuses a scan of another folder.

- A periodic in-process scan covers the whole root. The server listens
  while that scan runs. The timer and POST /rescan share one lock; a tick
  during a scan does nothing. Reads return the last committed inventory.
  The tick itself is not an action. Songs that changed are appended
  to the action log in the scan transaction.
- A failed scan commits nothing. Locations go absent only after the full
  walk reconciles.
- An unchanged scan writes nothing.
- A present file whose size and mtime match the stored values is not
  read again. mtime is a hint, not identity. inotify is later.
- This table is never sent to clients.

## 3. actions

One append-only log for catalog changes and user actions. Rows are kept.
About 25,000 songs makes a full replay unnecessary and a full resend
on every action wasteful, so devices hydrate once and then apply this log.

- seq INTEGER PRIMARY KEY
- action_id TEXT NOT NULL UNIQUE — client UUID for user actions, server UUID for catalog actions
- user_id TEXT — null for a catalog action
- device_id TEXT — null for a catalog action
- song_id TEXT NOT NULL REFERENCES inventory(song_id)
- action_type TEXT NOT NULL
- occurred_at_ms INTEGER — client clock, descriptive; null for a catalog action
- received_at_ms INTEGER NOT NULL — server clock, descriptive
- payload BLOB NOT NULL — SQLite JSONB

Catalog action_type values: song_upsert, song_unavailable.
User action_type values: play, finish, skip, like, unlike.

song_upsert payload is the device view of that song: hash, available,
metadata, and artwork hash when the file has a front cover.
song_unavailable carries available false. A path-only
move appends nothing. An unchanged scan appends nothing.

Ordering is seq. new state = f(state, action), applied in that order.
The accepting transaction appends the action, updates the song row or
that user's analytics, and acknowledges with seq.

Authenticate user_id. A library action has no user. A retry with the
same action_id and payload returns the existing seq and leaves state
unchanged. The same action_id with a different payload is an error.
User actions may reference unavailable songs.

A device pulls actions with seq greater than its cursor, keeping
catalog actions and its own user actions.

This spike accepts play, like, and unlike. Finish and skip are not
accepted until their meanings are defined.

A play is one accepted action. A second action id is a second play,
including two devices at once. payload listenedMs, when present, is
added to listened_ms. occurred_at_ms is stored as last_played_at_ms
and does not order anything.

Like sets liked to 1. Unlike sets it to 0. A later accepted action
replaces the earlier one. The user is the authenticated id, never a
user id in the body.

## 4. analytics

Per-user materialized state, rebuilt by the action reducer. Bootstrap
reads this table. Later sync applies actions, not this table.

- user_id TEXT NOT NULL
- song_id TEXT NOT NULL REFERENCES inventory(song_id)
- play_count INTEGER NOT NULL DEFAULT 0
- finished_count INTEGER NOT NULL DEFAULT 0
- skip_count INTEGER NOT NULL DEFAULT 0
- listened_ms INTEGER NOT NULL DEFAULT 0
- last_played_at_ms INTEGER — descriptive
- liked INTEGER — like and unlike folded in seq order

PRIMARY KEY (user_id, song_id).

The accept transaction updates this table. A conflicting action_id does not.

## 5. library_state

- singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1)
- library_id TEXT NOT NULL — UUID, fixed for this database

library_id does not change for the life of the database file.

## 6. Bootstrap

There is no stored snapshot and no snapshot timer. A joining device
reads catalog and its analytics in one database transaction. The first
read fixes the SQLite snapshot. seq in the response is the greatest
action seq visible in that snapshot. Song rows and analytics match it.

{
  "libraryId": "…",
  "seq": 480,
  "songs": [
    {
      "id": "…",
      "hash": "…",
      "available": true,
      "metadata": {},
      "artwork": "…",
      "analytics": {}
    }
  ]
}

Every song is included. A song this user has never played has empty
analytics. Paths are not included. artwork is omitted when the file has
no front cover. The image itself is GET /artwork/{hash}.

The device stores that state and seq together, then pulls actions with
a greater seq. Plays already folded into analytics are not in that
pull. A song added after the read arrives as a later song_upsert.

A changed library_id requires a new bootstrap. A cursor past the
greatest seq means this database was restored or replaced; the client
bootstraps again. A stored copy of the document is not part of this
design.

## Client storage

- Catalog state: id, hash, available, metadata, artwork hash, and this user's analytics.
- Pending actions, removed only after the seq acknowledgement.
- One cursor: seq from bootstrap, then the latest applied action.
- Local audio cache records.
- A full-text index derived from inventory metadata.

Do not copy file_location. Commit received state and its cursor together.
Keep unacknowledged local actions, and apply them on top after bootstrap.

## Playlists

Deferred until conflict rules are defined.

## Implementation

- Foreign keys on every SQLite connection.
- Database file on local disk, even when audio is on SMB.
- JSON expression indexes only for real filters.
- A separate full-text index for search.
- The library is rows, not one JSON document.
- No document-store or sync framework.
