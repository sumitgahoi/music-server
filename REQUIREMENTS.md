# Local-first music system

## Purpose

Build a personal music system with a Go/SQLite server and an
offline-first client. Reliability, simplicity, and maintainability
matter more than features.

## Architecture decisions

- Server: Go and SQLite, developed on a Debian Linux VM.
- Client: programs read the inventory as JSON. A native Swift iPhone
app with local SQLite comes later.
- A song is its song_id. Paths are not sent to devices. Hash identifies
the current bytes, with the in-place update in Library and identity.
- Catalog changes and user actions share one append-only log.
A device keeps one cursor.
- Clients work locally and synchronize asynchronously.
- The server durably stores accepted actions and canonical state.
- A device hydrates from one consistent read, then applies actions
after that cursor.
- Keep the SQLite database on local disk.

## Product requirements

### Library and identity

- Renaming or moving a file inside the root keeps its identity and
user state when the bytes are unchanged.
- Identical copies at different paths are the same asset, still found
at each path.
- A byte change at a path the song already occupies keeps the song
and updates its hash, when those bytes are not already another song
and the previous bytes are no longer in the library.
- A missing file is reported missing. Its row and history stay.
- Rescanning an unchanged library adds nothing.
- Read music files without modifying their contents or tags.
- A scan keeps the formats iPhone plays: MP3, AAC, Apple Lossless,
FLAC, WAV, AIFF, and Dolby Digital. Other files are left out.

MVP limitations, deferred to a later milestone:

- A move and a byte change between two scans, including a retag while
moving, are a new song. The old song stays missing and keeps its history.
- Replacing a file with bytes that already belong to another song
follows that song. The previous song becomes missing when it has no
copy left.
- Retagging one copy, while another copy of the old bytes remains,
splits that path into a new song.
- When one scan makes several songs newly byte-identical, or retags
several copies of one song into different bytes, the old songs stay
missing and new songs are created.

### Playback and offline operation

- Playback must not wait for user-state synchronization.
- Previously downloaded music must remain playable without a server.
- Target behavior: download on first play and play from local storage.
- Start with an LRU cache of approximately 100 songs.
- Active playback and in-progress downloads must be protected
from eviction.
- Playback and cache implementation follow the identity/sync prototype.

### User state

- Preserve plays, likes, and playlists across device changes.
- Keep each user's state isolated.
- Record offline changes in a durable local queue of actions.
- Give actions stable IDs so retries cannot apply them twice.
- Preserve distinct legitimate plays, including simultaneous plays
from different devices.
- Like and unlike fold in server accept order. That order is the only
conflict rule. Client clocks are descriptive. Define playlist conflict
behavior before implementing playlists.
- Acknowledge events only after durable server persistence.

### Synchronization and recovery

- Support retries after timeouts, disconnects, and process restarts.
- One server sequence orders catalog changes and user actions.
- Commit action acceptance, derived state, and ordering consistently.
- Advance the client cursor only with the actions it has stored.
- Bootstrap reads the catalog and that user's analytics in one
database snapshot, at one seq, then applies later actions. That read
must not double-count a play already included in analytics, and must
not miss a song added after it.
- Preserve pending local actions during bootstrap or recovery.
- A successful retry must return an unambiguous acknowledgement.
- Detect an invalid cursor or changed server history and recover
explicitly rather than silently skipping data.
- A new phone must recover synchronized user state from the server.
Unsynchronized events cannot be recovered from a lost device.

## Development strategy

Prove architecture risks before building a polished player.

### Milestone 1: library inventory

Choose one music folder, scan it, and keep a durable inventory.
Programs read that inventory as JSON.

Acceptance:

- Initial import assigns stable asset identities.
- An unchanged rescan creates no duplicate assets.
- Moving or renaming a file inside the root preserves identity when
the bytes are unchanged, and reports neither a new asset nor a deletion.
- Replacing the bytes at a song's path keeps the identity and updates
the hash, when those bytes are new and the previous bytes are gone.
- A move together with a byte change is a new song. The old song is
reported missing and its row is kept.
- Identical copies share identity while remaining at each path.
- Missing files are reported without deleting history.
- An inventory that fails or stops early does not report files as
added or removed.
- Source files are never modified.
- The JSON lists each asset once, where it is now, and which files
are missing.

Use copied sample files, not the production music collection.
No playback, accounts, synchronization, or the iPhone app in this
milestone.

### Milestone 2: synchronization spike — current task

Use one Go server and two lightweight simulated clients with separate
SQLite databases and durable event queues.

Begin with play events and a like/unlike operation. Defer playlists
until the core protocol works.

Prove:

- Two offline clients of one user converge after reconnecting. A second
server-bound user stays isolated. A user ID in the payload is not
that binding.
- A song added after bootstrap arrives as a later action on the same
cursor as plays and likes.
- Resending an accepted action, or reapplying a received one, does not
apply it twice.
- A lost response after server commit is safe to retry.
- Pending client actions survive restart.
- Server state survives restart.
- Conflicting likes resolve by server accept order.
- Bootstrap reads catalog and analytics at one seq during concurrent
writes, then catches up from the action log.
- Client recovery preserves pending local actions.

The simulator proves protocol behavior; native iOS validation follows.

### Later milestones

- Search and audio delivery.
- A thin native iPhone client proving actual offline playback and sync.
- Cache management and everyday usability.
- Playlists and additional features only after the foundation works.

## Non-goals for the prototype

- Replacing or adapting the standalone OpenSubsonic client.
- Spotify Connect-style remote playback.
- Recommendations, YouTube integration, or polished UI.
- Microservices, brokers, distributed databases, or plugin frameworks.
- Production deployment, public exposure, or production data migration.