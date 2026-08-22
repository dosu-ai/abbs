// Port of the schema const in internal/store/store.go, with the documented
// deltas (cfworker/PLAN.md "Schema port"):
//   - pragmas dropped (DO storage owns journaling/durability)
//   - idempotency.created_ns (UnixNano) → created_ms (Date.now()): nanos
//     exceed 2^53 and would corrupt the retention comparison as a JS number
//   - idempotency.body BLOB → TEXT (replay stores the exact JSON string)
//   - the mentions-column migration is folded into the base DDL (fresh
//     implementation, no legacy databases)

export const SCHEMA = `
CREATE TABLE IF NOT EXISTS events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	type        TEXT NOT NULL,
	thread_id   TEXT,
	occurred_at TEXT NOT NULL,
	payload     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
	username     TEXT PRIMARY KEY,
	kind         TEXT NOT NULL,
	display_name TEXT,
	owned_by     TEXT,
	admin        INTEGER NOT NULL DEFAULT 0,
	deactivated  INTEGER NOT NULL DEFAULT 0,
	created_at   TEXT NOT NULL,
	token_hash   TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS threads (
	id                TEXT PRIMARY KEY,
	kind              TEXT NOT NULL,
	title             TEXT NOT NULL,
	tags              TEXT NOT NULL,
	creator           TEXT NOT NULL,
	created_at        TEXT NOT NULL,
	created_seq       INTEGER NOT NULL,
	last_activity_seq INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS thread_participants (
	thread_id TEXT NOT NULL,
	username  TEXT NOT NULL,
	PRIMARY KEY (thread_id, username)
);
CREATE TABLE IF NOT EXISTS messages (
	id          TEXT PRIMARY KEY,
	thread_id   TEXT NOT NULL,
	author      TEXT NOT NULL,
	content     TEXT,
	mentions    TEXT,
	deleted     INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	edited_at   TEXT,
	deleted_at  TEXT,
	deleted_by  TEXT,
	created_seq INTEGER NOT NULL,
	seq         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages (thread_id, created_seq);
CREATE INDEX IF NOT EXISTS idx_threads_activity ON threads (last_activity_seq);
CREATE TABLE IF NOT EXISTS thread_tags (
	thread_id TEXT NOT NULL,
	tag       TEXT NOT NULL,
	PRIMARY KEY (thread_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_thread_tags_tag ON thread_tags (tag);
CREATE TABLE IF NOT EXISTS mentions (
	message_id TEXT NOT NULL,
	thread_id  TEXT NOT NULL,
	username   TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	PRIMARY KEY (message_id, username)
);
CREATE INDEX IF NOT EXISTS idx_mentions_user ON mentions (username, seq);
CREATE TABLE IF NOT EXISTS read_cursors (
	username  TEXT NOT NULL,
	thread_id TEXT NOT NULL,
	seq       INTEGER NOT NULL,
	PRIMARY KEY (username, thread_id)
);
CREATE TABLE IF NOT EXISTS reactions (
	message_id TEXT NOT NULL,
	username   TEXT NOT NULL,
	emoji      TEXT NOT NULL,
	created_at TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	PRIMARY KEY (message_id, username, emoji)
);
CREATE INDEX IF NOT EXISTS idx_reactions_seq ON reactions (message_id, seq);
CREATE TABLE IF NOT EXISTS tag_subscriptions (
	username TEXT NOT NULL,
	tag      TEXT NOT NULL,
	PRIMARY KEY (username, tag)
);
CREATE TABLE IF NOT EXISTS idempotency (
	principal    TEXT NOT NULL,
	endpoint     TEXT NOT NULL,
	key          TEXT NOT NULL,
	request_hash TEXT NOT NULL,
	status       INTEGER NOT NULL,
	content_type TEXT NOT NULL,
	body         TEXT NOT NULL,
	created_ms   INTEGER NOT NULL,
	PRIMARY KEY (principal, endpoint, key)
);
`;
