// Package store is the SQLite storage backend: a single append-only events
// table whose AUTOINCREMENT primary key is the global monotonic sequence —
// that column *is* the cursor — plus current-state tables for users,
// threads, and messages. SQLite's serialized writes make sequence order
// equal commit order for free (the property M6 must reconstruct on Postgres
// with pg_advisory_xact_lock).
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/dosu-ai/abbs/internal/api"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrUsernameTaken = errors.New("username taken")
)

// ErrUnknownParticipant reports a DM participant that is not a user.
type ErrUnknownParticipant struct{ Username string }

func (e ErrUnknownParticipant) Error() string {
	return fmt.Sprintf("unknown participant %q", e.Username)
}

const schema = `
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
`

type Store struct {
	db *sql.DB
	// mu serializes writes: one writer at a time keeps SQLITE_BUSY out of
	// the picture and makes "insert event, then update state" atomic per
	// request without retry loops.
	mu     sync.Mutex
	wakeup *broadcast
}

func Open(path string) (*Store, error) {
	// WAL for concurrent readers during writes; synchronous=FULL so a
	// kill -9 never loses an acknowledged write (the M2 exit criterion).
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, wakeup: newBroadcast()}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Wakeup returns a channel closed when the next event is appended.
func (s *Store) Wakeup() <-chan struct{} { return s.wakeup.wait() }

func seqToken(seq int64) string { return strconv.FormatInt(seq, 10) }

// ParseSeq parses an opaque cursor token. Cursors are opaque to clients,
// not to us: they are the decimal event sequence.
func ParseSeq(token string) (int64, error) {
	n, err := strconv.ParseInt(token, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid cursor %q", token)
	}
	return n, nil
}

func now(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err) // crypto/rand failure is not a recoverable request error
	}
	return id.String()
}

// insertEvent appends an event row and returns its sequence. The payload is
// the event's extra fields; seq/type/occurred_at are merged in at read time.
func insertEvent(tx *sql.Tx, typ string, threadID *string, occurredAt string, extra map[string]any) (int64, error) {
	payload, err := json.Marshal(extra)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO events (type, thread_id, occurred_at, payload) VALUES (?, ?, ?, ?)`,
		typ, threadID, occurredAt, string(payload))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func updateEventPayload(tx *sql.Tx, seq int64, extra map[string]any) error {
	payload, err := json.Marshal(extra)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE events SET payload = ? WHERE seq = ?`, string(payload), seq)
	return err
}

// ClaimUser implements first-claim-wins. tokenHash is the SHA-256 hex of the
// bearer token — tokens are stored hashed, introspection is a lookup.
func (s *Store) ClaimUser(username, kind string, displayName *string, tokenHash string, at time.Time) (api.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return api.User{}, err
	}
	defer tx.Rollback()

	ts := now(at)
	if _, err := tx.Exec(
		`INSERT INTO users (username, kind, display_name, created_at, token_hash) VALUES (?, ?, ?, ?, ?)`,
		username, kind, displayName, ts, tokenHash,
	); err != nil {
		if strings.Contains(err.Error(), "users.username") {
			return api.User{}, ErrUsernameTaken
		}
		return api.User{}, err
	}
	user := api.User{Username: username, Kind: kind, DisplayName: displayName, CreatedAt: ts}
	if _, err := insertEvent(tx, "user.created", nil, ts, map[string]any{"user": user}); err != nil {
		return api.User{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.User{}, err
	}
	s.wakeup.notify()
	return user, nil
}

// UserByTokenHash resolves a bearer credential to its principal.
func (s *Store) UserByTokenHash(tokenHash string) (api.User, error) {
	row := s.db.QueryRow(
		`SELECT username, kind, display_name, owned_by, admin, deactivated, created_at FROM users WHERE token_hash = ?`,
		tokenHash)
	return scanUser(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(row rowScanner) (api.User, error) {
	var u api.User
	var displayName, ownedBy sql.NullString
	var admin, deactivated int
	err := row.Scan(&u.Username, &u.Kind, &displayName, &ownedBy, &admin, &deactivated, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return api.User{}, ErrNotFound
	}
	if err != nil {
		return api.User{}, err
	}
	if displayName.Valid {
		u.DisplayName = &displayName.String
	}
	if ownedBy.Valid {
		u.OwnedBy = &ownedBy.String
	}
	u.Admin = admin != 0
	u.Deactivated = deactivated != 0
	return u, nil
}

// CreateThread creates a thread and its first message in one transaction.
// A non-nil participants set makes it a DM whose membership (participants ∪
// creator) is permanently fixed. Tags must already be normalized.
func (s *Store) CreateThread(creator, title, content string, tags, participants []string, at time.Time) (api.Thread, api.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return api.Thread{}, api.Message{}, err
	}
	defer tx.Rollback()

	kind := "public"
	if len(participants) > 0 {
		kind = "dm"
		for _, p := range participants {
			var one int
			err := tx.QueryRow(`SELECT 1 FROM users WHERE username = ?`, p).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return api.Thread{}, api.Message{}, ErrUnknownParticipant{Username: p}
			}
			if err != nil {
				return api.Thread{}, api.Message{}, err
			}
		}
	}

	ts := now(at)
	threadID, messageID := newID(), newID()

	// Two placeholder events first: their AUTOINCREMENT seqs are needed
	// inside the payloads (created_seq, last_activity_seq), so the payloads
	// are filled in below, within the same transaction.
	threadSeq, err := insertEvent(tx, "thread.created", &threadID, ts, nil)
	if err != nil {
		return api.Thread{}, api.Message{}, err
	}
	messageSeq, err := insertEvent(tx, "message.created", &threadID, ts, nil)
	if err != nil {
		return api.Thread{}, api.Message{}, err
	}

	if tags == nil {
		tags = []string{}
	}
	thread := api.Thread{
		ID:              threadID,
		Kind:            kind,
		Title:           title,
		Tags:            tags,
		Creator:         creator,
		Participants:    participants,
		CreatedAt:       ts,
		CreatedSeq:      seqToken(threadSeq),
		LastActivitySeq: seqToken(messageSeq),
	}
	msg := api.Message{
		ID:        messageID,
		ThreadID:  threadID,
		Author:    creator,
		Content:   content,
		CreatedAt: ts,
		Seq:       seqToken(messageSeq),
		Reactions: []api.ReactionTally{},
	}
	if err := updateEventPayload(tx, threadSeq, map[string]any{"thread": thread}); err != nil {
		return api.Thread{}, api.Message{}, err
	}
	if err := updateEventPayload(tx, messageSeq, map[string]any{"message": msg}); err != nil {
		return api.Thread{}, api.Message{}, err
	}

	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return api.Thread{}, api.Message{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO threads (id, kind, title, tags, creator, created_at, created_seq, last_activity_seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		threadID, kind, title, string(tagsJSON), creator, ts, threadSeq, messageSeq,
	); err != nil {
		return api.Thread{}, api.Message{}, err
	}
	for _, p := range participants {
		if _, err := tx.Exec(`INSERT INTO thread_participants (thread_id, username) VALUES (?, ?)`, threadID, p); err != nil {
			return api.Thread{}, api.Message{}, err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO messages (id, thread_id, author, content, created_at, created_seq, seq) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		messageID, threadID, creator, content, ts, messageSeq, messageSeq,
	); err != nil {
		return api.Thread{}, api.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Thread{}, api.Message{}, err
	}
	s.wakeup.notify()
	return thread, msg, nil
}

// GetThread returns a thread if the viewer may see it. DM threads are
// invisible to non-participants: ErrNotFound, never a 403 — existence is
// not leaked.
func (s *Store) GetThread(id, viewer string) (api.Thread, error) {
	return getThread(s.db, id, viewer)
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func getThread(q querier, id, viewer string) (api.Thread, error) {
	var t api.Thread
	var tagsJSON string
	var createdSeq, lastActivitySeq int64
	err := q.QueryRow(
		`SELECT id, kind, title, tags, creator, created_at, created_seq, last_activity_seq FROM threads WHERE id = ?`, id,
	).Scan(&t.ID, &t.Kind, &t.Title, &tagsJSON, &t.Creator, &t.CreatedAt, &createdSeq, &lastActivitySeq)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Thread{}, ErrNotFound
	}
	if err != nil {
		return api.Thread{}, err
	}
	if err := json.Unmarshal([]byte(tagsJSON), &t.Tags); err != nil {
		return api.Thread{}, err
	}
	t.CreatedSeq = seqToken(createdSeq)
	t.LastActivitySeq = seqToken(lastActivitySeq)
	if t.Kind == "dm" {
		rows, err := q.Query(`SELECT username FROM thread_participants WHERE thread_id = ? ORDER BY username`, id)
		if err != nil {
			return api.Thread{}, err
		}
		defer rows.Close()
		member := false
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return api.Thread{}, err
			}
			t.Participants = append(t.Participants, p)
			member = member || p == viewer
		}
		if err := rows.Err(); err != nil {
			return api.Thread{}, err
		}
		if !member {
			return api.Thread{}, ErrNotFound
		}
	}
	return t, nil
}

// PostMessage appends a message to a thread the author can see, advancing
// the thread's activity cursor.
func (s *Store) PostMessage(threadID, author, content string, at time.Time) (api.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()

	if _, err := getThread(tx, threadID, author); err != nil {
		return api.Message{}, err
	}

	ts := now(at)
	seq, err := insertEvent(tx, "message.created", &threadID, ts, nil)
	if err != nil {
		return api.Message{}, err
	}
	msg := api.Message{
		ID:        newID(),
		ThreadID:  threadID,
		Author:    author,
		Content:   content,
		CreatedAt: ts,
		Seq:       seqToken(seq),
		Reactions: []api.ReactionTally{},
	}
	if err := updateEventPayload(tx, seq, map[string]any{"message": msg}); err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO messages (id, thread_id, author, content, created_at, created_seq, seq) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, threadID, author, content, ts, seq, seq,
	); err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ?`, seq, threadID); err != nil {
		return api.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, err
	}
	s.wakeup.notify()
	return msg, nil
}

// ListMessages pages through a thread's messages in creation order. after
// is the created_seq page anchor (0 = from the start); nextPage is the
// anchor for the following page, nil on the last one.
func (s *Store) ListMessages(threadID, viewer string, after int64, limit int) (items []api.Message, nextPage *string, asOf string, err error) {
	if _, err := s.GetThread(threadID, viewer); err != nil {
		return nil, nil, "", err
	}
	cur, err := s.CurrentSeq()
	if err != nil {
		return nil, nil, "", err
	}
	asOf = seqToken(cur)

	rows, err := s.db.Query(
		`SELECT id, thread_id, author, content, deleted, created_at, edited_at, deleted_at, deleted_by, created_seq, seq
		 FROM messages WHERE thread_id = ? AND created_seq > ? ORDER BY created_seq LIMIT ?`,
		threadID, after, limit+1)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()

	items = []api.Message{}
	createdSeqs := []int64{}
	for rows.Next() {
		var m api.Message
		var content, editedAt, deletedAt, deletedBy sql.NullString
		var deleted int
		var createdSeq, seq int64
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Author, &content, &deleted, &m.CreatedAt, &editedAt, &deletedAt, &deletedBy, &createdSeq, &seq); err != nil {
			return nil, nil, "", err
		}
		m.Deleted = deleted != 0
		if !m.Deleted {
			m.Content = content.String
		}
		if editedAt.Valid {
			m.EditedAt = &editedAt.String
		}
		m.DeletedAt = deletedAt.String
		m.DeletedBy = deletedBy.String
		m.Seq = seqToken(seq)
		m.Reactions = []api.ReactionTally{}
		items = append(items, m)
		createdSeqs = append(createdSeqs, createdSeq)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if len(items) > limit {
		// The extra row only proved another page exists; the anchor is the
		// created_seq of the last item kept (stable across edits, unlike seq).
		items = items[:limit]
		tok := seqToken(createdSeqs[limit-1])
		nextPage = &tok
	}
	return items, nextPage, asOf, nil
}

// CurrentSeq is the newest sequence in the log (0 when empty).
func (s *Store) CurrentSeq() (int64, error) {
	var cur int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&cur)
	return cur, err
}

// Events returns up to limit events after the cursor, restricted to the
// viewer's visible slice: everything except DM threads they are not in.
// The returned cursor is the last event's seq, or the request cursor when
// the batch is empty (the dumb-and-safe echo).
func (s *Store) Events(viewer string, after int64, limit int) ([]api.Event, string, error) {
	rows, err := s.db.Query(
		`SELECT e.seq, e.type, e.occurred_at, e.payload
		 FROM events e LEFT JOIN threads t ON t.id = e.thread_id
		 WHERE e.seq > ?
		   AND (e.thread_id IS NULL OR t.kind = 'public'
		        OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = e.thread_id AND p.username = ?))
		 ORDER BY e.seq LIMIT ?`,
		after, viewer, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	events := []api.Event{}
	cursor := after
	for rows.Next() {
		var seq int64
		var typ, occurredAt, payload string
		if err := rows.Scan(&seq, &typ, &occurredAt, &payload); err != nil {
			return nil, "", err
		}
		ev := api.Event{}
		if payload != "" && payload != "null" {
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				return nil, "", err
			}
		}
		ev["seq"] = seqToken(seq)
		ev["type"] = typ
		ev["occurred_at"] = occurredAt
		events = append(events, ev)
		cursor = seq
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return events, seqToken(cursor), nil
}
