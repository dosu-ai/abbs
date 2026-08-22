// Package cache is the Go client's read cache (M7): a cursor-replay loop
// into a per-principal SQLite file. The events endpoint is the sync
// protocol — apply events to local SQLite and save the new cursor in the
// same transaction. The cache is the principal's visible slice, never the
// database (the server's events endpoint enforces DM privacy); it is
// disposable by construction — delete the file and it rebuilds via
// snapshot-then-tail bootstrap.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS threads (
	id                TEXT PRIMARY KEY,
	kind              TEXT NOT NULL,
	title             TEXT NOT NULL,
	creator           TEXT NOT NULL,
	tags              TEXT NOT NULL, -- JSON array
	participants      TEXT NOT NULL, -- JSON array
	created_at        TEXT NOT NULL,
	created_seq       INTEGER NOT NULL,
	last_activity_seq INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS threads_activity ON threads (last_activity_seq);
CREATE TABLE IF NOT EXISTS messages (
	id         TEXT PRIMARY KEY,
	thread_id  TEXT NOT NULL,
	author     TEXT NOT NULL,
	content    TEXT NOT NULL,
	mentions   TEXT NOT NULL, -- JSON array
	deleted    INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	edited_at  TEXT,
	deleted_at TEXT NOT NULL DEFAULT '',
	deleted_by TEXT NOT NULL DEFAULT '',
	seq        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_thread ON messages (thread_id, seq);
CREATE TABLE IF NOT EXISTS reactions (
	message_id TEXT NOT NULL,
	username   TEXT NOT NULL,
	emoji      TEXT NOT NULL,
	PRIMARY KEY (message_id, username, emoji)
);
`

// Cache is one workspace's local read cache for one principal.
type Cache struct {
	db *sql.DB
}

func Open(path string) (*Cache, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// The cache has a single writer (the sync loop); serialized access keeps
	// modernc.org/sqlite happy without a connection pool.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("cache schema: %w", err)
	}
	return &Cache{db: db}, nil
}

func (c *Cache) Close() error { return c.db.Close() }

// Cursor returns the saved replay cursor; ok is false when the cache has
// never been bootstrapped (empty or freshly recreated file).
func (c *Cache) Cursor() (cursor string, ok bool, err error) {
	err = c.db.QueryRow(`SELECT value FROM meta WHERE key = 'cursor'`).Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return cursor, err == nil, err
}

// seqInt parses a wire cursor token. Tokens are opaque to protocol clients,
// but the cache is our own implementation detail and may rely on our
// server's integer encoding for ordering guards.
func seqInt(tok string) int64 {
	n, _ := strconv.ParseInt(tok, 10, 64)
	return n
}

func jsonArr(ss []string) string {
	if ss == nil {
		ss = []string{}
	}
	b, _ := json.Marshal(ss)
	return string(b)
}

func upsertThread(tx *sql.Tx, t api.Thread) error {
	_, err := tx.Exec(`INSERT INTO threads (id, kind, title, creator, tags, participants, created_at, created_seq, last_activity_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title, tags = excluded.tags,
			last_activity_seq = excluded.last_activity_seq
		WHERE excluded.last_activity_seq >= threads.last_activity_seq`,
		t.ID, t.Kind, t.Title, t.Creator, jsonArr(t.Tags), jsonArr(t.Participants),
		t.CreatedAt, seqInt(t.CreatedSeq), seqInt(t.LastActivitySeq))
	return err
}

func upsertMessage(tx *sql.Tx, m api.Message) error {
	var edited any
	if m.EditedAt != nil {
		edited = *m.EditedAt
	}
	if _, err := tx.Exec(`INSERT INTO messages (id, thread_id, author, content, mentions, deleted, created_at, edited_at, deleted_at, deleted_by, seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			content = excluded.content, mentions = excluded.mentions,
			deleted = excluded.deleted, edited_at = excluded.edited_at,
			deleted_at = excluded.deleted_at, deleted_by = excluded.deleted_by,
			seq = excluded.seq
		WHERE excluded.seq >= messages.seq`,
		m.ID, m.ThreadID, m.Author, m.Content, jsonArr(m.Mentions), m.Deleted,
		m.CreatedAt, edited, m.DeletedAt, m.DeletedBy, seqInt(m.Seq)); err != nil {
		return err
	}
	// A message event advances its thread's activity cursor.
	_, err := tx.Exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ? AND last_activity_seq < ?`,
		seqInt(m.Seq), m.ThreadID, seqInt(m.Seq))
	return err
}

// decode re-marshals one event payload into a typed shape; ok is false when
// the payload doesn't fit (a malformed or half-known event — skipped, never
// fatal, per the evolution rules).
func decode[T any](ev api.Event, key string) (T, bool) {
	var out T
	raw, exists := ev[key]
	if !exists {
		return out, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, false
	}
	return out, true
}

func str(ev api.Event, key string) string {
	s, _ := ev[key].(string)
	return s
}

// Apply replays one event batch into the cache and checkpoints the cursor
// in the same transaction. Unknown event types and unknown fields on known
// types are ignored while the cursor still advances past them — the
// evolution-rule contract that keeps deployed cache loops alive when new
// event types ship.
func (c *Cache) Apply(batch api.EventBatch) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, ev := range batch.Events {
		switch str(ev, "type") {
		case "thread.created":
			if t, ok := decode[api.Thread](ev, "thread"); ok && t.ID != "" {
				if err := upsertThread(tx, t); err != nil {
					return err
				}
			}
		case "thread.tags_changed":
			if id := str(ev, "thread_id"); id != "" {
				if tags, ok := decode[[]string](ev, "tags"); ok {
					seq := seqInt(str(ev, "seq"))
					if _, err := tx.Exec(`UPDATE threads SET tags = ?, last_activity_seq = max(last_activity_seq, ?) WHERE id = ?`,
						jsonArr(tags), seq, id); err != nil {
						return err
					}
				}
			}
		case "message.created", "message.edited", "message.deleted":
			// All three carry the message's full current state — application
			// is an upsert, which is what makes overlap-tolerant replay work.
			if m, ok := decode[api.Message](ev, "message"); ok && m.ID != "" {
				if err := upsertMessage(tx, m); err != nil {
					return err
				}
			}
		case "reaction.added":
			if mid, u, e := str(ev, "message_id"), str(ev, "username"), str(ev, "emoji"); mid != "" && u != "" && e != "" {
				if _, err := tx.Exec(`INSERT OR IGNORE INTO reactions (message_id, username, emoji) VALUES (?, ?, ?)`, mid, u, e); err != nil {
					return err
				}
			}
		case "reaction.removed":
			if mid, u, e := str(ev, "message_id"), str(ev, "username"), str(ev, "emoji"); mid != "" && u != "" && e != "" {
				if _, err := tx.Exec(`DELETE FROM reactions WHERE message_id = ? AND username = ? AND emoji = ?`, mid, u, e); err != nil {
					return err
				}
			}
		default:
			// Unknown type: skip the payload, advance the cursor (below).
		}
	}
	if err := setMeta(tx, "cursor", batch.Cursor); err != nil {
		return err
	}
	return tx.Commit()
}

func setMeta(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Bootstrap is the snapshot-then-tail stitch: fetch current state via the
// normal paginated read endpoints, note the sequence at snapshot time (the
// first page's as_of), and set the replay cursor there. Events between the
// anchor and the end of the snapshot will be re-applied by the tail — the
// upsert guards make that harmless.
func (c *Cache) Bootstrap(ctx context.Context, cl *client.Client) error {
	anchor := ""
	var threads []api.Thread
	for page := ""; ; {
		tp, err := cl.ListThreads(ctx, client.ListThreadsOptions{Page: page, Limit: 100})
		if err != nil {
			return fmt.Errorf("bootstrap threads: %w", err)
		}
		if anchor == "" {
			anchor = tp.AsOf
		}
		threads = append(threads, tp.Items...)
		if tp.NextPage == nil {
			break
		}
		page = *tp.NextPage
	}

	var messages []api.Message
	var reactions []struct{ messageID, username, emoji string }
	for _, t := range threads {
		for page := ""; ; {
			mp, err := cl.ListMessages(ctx, t.ID, page, 100)
			if err != nil {
				return fmt.Errorf("bootstrap messages for thread %s: %w", t.ID, err)
			}
			messages = append(messages, mp.Items...)
			if mp.NextPage == nil {
				break
			}
			page = *mp.NextPage
		}
	}
	// Per-user reaction rows (the idempotent form) exist only on the
	// reactions endpoint; fetch them just for messages that carry any.
	for _, m := range messages {
		if len(m.Reactions) == 0 {
			continue
		}
		for page := ""; ; {
			rp, err := cl.ListReactions(ctx, m.ID, page, 100)
			if err != nil {
				return fmt.Errorf("bootstrap reactions for message %s: %w", m.ID, err)
			}
			for _, r := range rp.Items {
				reactions = append(reactions, struct{ messageID, username, emoji string }{m.ID, r.Username, r.Emoji})
			}
			if rp.NextPage == nil {
				break
			}
			page = *rp.NextPage
		}
	}

	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range threads {
		if err := upsertThread(tx, t); err != nil {
			return err
		}
	}
	for _, m := range messages {
		if err := upsertMessage(tx, m); err != nil {
			return err
		}
	}
	for _, r := range reactions {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO reactions (message_id, username, emoji) VALUES (?, ?, ?)`,
			r.messageID, r.username, r.emoji); err != nil {
			return err
		}
	}
	if err := setMeta(tx, "cursor", anchor); err != nil {
		return err
	}
	return tx.Commit()
}
