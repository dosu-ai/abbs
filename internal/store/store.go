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
	"regexp"
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
`

// migrations are idempotent column additions the base schema (CREATE TABLE
// IF NOT EXISTS) cannot express; "duplicate column" errors are expected.
var migrations = []string{
	`ALTER TABLE messages ADD COLUMN mentions TEXT`,
}

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
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("apply migration %q: %w", m, err)
		}
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

// mentionRE finds @username candidates: an @ not embedded in a word (so
// emails don't match), followed by a well-formed username.
var mentionRE = regexp.MustCompile(`(?:^|[^a-zA-Z0-9._@-])@([a-z0-9][a-z0-9._-]{0,31})`)

// extractMentions resolves @mention candidates in markdown against the
// users table; only existing usernames survive. A candidate that fails to
// resolve is retried with trailing punctuation-ish characters trimmed, so
// "ask @bob." mentions bob.
func extractMentions(tx *sql.Tx, content string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionRE.FindAllStringSubmatch(content, -1) {
		for _, cand := range []string{m[1], strings.TrimRight(m[1], "._-")} {
			if cand == "" || seen[cand] {
				break
			}
			var one int
			err := tx.QueryRow(`SELECT 1 FROM users WHERE username = ?`, cand).Scan(&one)
			if err == nil {
				seen[cand] = true
				out = append(out, cand)
				break
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
	}
	return out, nil
}

func insertMentions(tx *sql.Tx, messageID, threadID string, usernames []string, seq int64) error {
	for _, u := range usernames {
		if _, err := tx.Exec(`INSERT INTO mentions (message_id, thread_id, username, seq) VALUES (?, ?, ?, ?)`,
			messageID, threadID, u, seq); err != nil {
			return err
		}
	}
	return nil
}

// advanceReadCursor moves a user's read cursor forward (never backward) —
// used so one's own posts don't land in one's own inbox. The manual PUT
// (SetReadCursor) is absolute instead and may move backward.
func advanceReadCursor(tx *sql.Tx, username, threadID string, seq int64) error {
	_, err := tx.Exec(
		`INSERT INTO read_cursors (username, thread_id, seq) VALUES (?, ?, ?)
		 ON CONFLICT (username, thread_id) DO UPDATE SET seq = excluded.seq WHERE excluded.seq > read_cursors.seq`,
		username, threadID, seq)
	return err
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
	mentioned, err := extractMentions(tx, content)
	if err != nil {
		return api.Thread{}, api.Message{}, err
	}
	msg := api.Message{
		ID:        messageID,
		ThreadID:  threadID,
		Author:    creator,
		Content:   content,
		Mentions:  mentioned,
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
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO thread_tags (thread_id, tag) VALUES (?, ?)`, threadID, tag); err != nil {
			return api.Thread{}, api.Message{}, err
		}
	}
	mentionsJSON, err := json.Marshal(mentioned)
	if err != nil {
		return api.Thread{}, api.Message{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO messages (id, thread_id, author, content, mentions, created_at, created_seq, seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, threadID, creator, content, string(mentionsJSON), ts, messageSeq, messageSeq,
	); err != nil {
		return api.Thread{}, api.Message{}, err
	}
	if err := insertMentions(tx, messageID, threadID, mentioned, messageSeq); err != nil {
		return api.Thread{}, api.Message{}, err
	}
	if err := advanceReadCursor(tx, creator, threadID, messageSeq); err != nil {
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

// scanThread scans the canonical thread column list:
// id, kind, title, tags, creator, created_at, created_seq, last_activity_seq.
// Participants are loaded separately.
func scanThread(row rowScanner) (api.Thread, error) {
	var t api.Thread
	var tagsJSON string
	var createdSeq, lastActivitySeq int64
	err := row.Scan(&t.ID, &t.Kind, &t.Title, &tagsJSON, &t.Creator, &t.CreatedAt, &createdSeq, &lastActivitySeq)
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
	return t, nil
}

func loadParticipants(q querier, threadID string) ([]string, error) {
	rows, err := q.Query(`SELECT username FROM thread_participants WHERE thread_id = ? ORDER BY username`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func getThread(q querier, id, viewer string) (api.Thread, error) {
	t, err := scanThread(q.QueryRow(
		`SELECT id, kind, title, tags, creator, created_at, created_seq, last_activity_seq FROM threads WHERE id = ?`, id))
	if err != nil {
		return api.Thread{}, err
	}
	if t.Kind == "dm" {
		if t.Participants, err = loadParticipants(q, id); err != nil {
			return api.Thread{}, err
		}
		member := false
		for _, p := range t.Participants {
			member = member || p == viewer
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
	mentioned, err := extractMentions(tx, content)
	if err != nil {
		return api.Message{}, err
	}
	msg := api.Message{
		ID:        newID(),
		ThreadID:  threadID,
		Author:    author,
		Content:   content,
		Mentions:  mentioned,
		CreatedAt: ts,
		Seq:       seqToken(seq),
		Reactions: []api.ReactionTally{},
	}
	if err := updateEventPayload(tx, seq, map[string]any{"message": msg}); err != nil {
		return api.Message{}, err
	}
	mentionsJSON, err := json.Marshal(mentioned)
	if err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO messages (id, thread_id, author, content, mentions, created_at, created_seq, seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, threadID, author, content, string(mentionsJSON), ts, seq, seq,
	); err != nil {
		return api.Message{}, err
	}
	if err := insertMentions(tx, msg.ID, threadID, mentioned, seq); err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ?`, seq, threadID); err != nil {
		return api.Message{}, err
	}
	if err := advanceReadCursor(tx, author, threadID, seq); err != nil {
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
		`SELECT id, thread_id, author, content, mentions, deleted, created_at, edited_at, deleted_at, deleted_by, created_seq, seq
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
		var content, mentions, editedAt, deletedAt, deletedBy sql.NullString
		var deleted int
		var createdSeq, seq int64
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Author, &content, &mentions, &deleted, &m.CreatedAt, &editedAt, &deletedAt, &deletedBy, &createdSeq, &seq); err != nil {
			return nil, nil, "", err
		}
		m.Deleted = deleted != 0
		if !m.Deleted {
			m.Content = content.String
			if mentions.Valid && mentions.String != "" {
				if err := json.Unmarshal([]byte(mentions.String), &m.Mentions); err != nil {
					return nil, nil, "", err
				}
			}
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

// ListThreads pages through the viewer's visible threads, most recent
// activity first. since=0 means no lower bound; before=0 (the page anchor)
// means start from the top; tags narrows to threads carrying any of them.
func (s *Store) ListThreads(viewer string, since, before int64, tags []string, limit int) (items []api.Thread, nextPage *string, asOf string, err error) {
	cur, err := s.CurrentSeq()
	if err != nil {
		return nil, nil, "", err
	}
	asOf = seqToken(cur)

	query := `SELECT id, kind, title, tags, creator, created_at, created_seq, last_activity_seq FROM threads t
		 WHERE (t.kind = 'public' OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = t.id AND p.username = ?))`
	args := []any{viewer}
	if since > 0 {
		query += ` AND t.last_activity_seq > ?`
		args = append(args, since)
	}
	if before > 0 {
		query += ` AND t.last_activity_seq < ?`
		args = append(args, before)
	}
	if len(tags) > 0 {
		query += ` AND EXISTS (SELECT 1 FROM thread_tags tt WHERE tt.thread_id = t.id AND tt.tag IN (?` + strings.Repeat(", ?", len(tags)-1) + `))`
		for _, tag := range tags {
			args = append(args, tag)
		}
	}
	query += ` ORDER BY t.last_activity_seq DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()

	items = []api.Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, nil, "", err
		}
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		tok := items[limit-1].LastActivitySeq
		nextPage = &tok
	}
	for i := range items {
		if items[i].Kind == "dm" {
			if items[i].Participants, err = loadParticipants(s.db, items[i].ID); err != nil {
				return nil, nil, "", err
			}
		}
	}
	return items, nextPage, asOf, nil
}

// Inbox pages through "what needs me": visible threads with activity past
// the viewer's read cursor, where the viewer is a DM participant, a public-
// thread participant (creator or has posted), or has an unread mention.
// Ordered by most recent activity first; before is the page anchor.
func (s *Store) Inbox(viewer string, before int64, limit int) (items []api.InboxItem, nextPage *string, asOf string, err error) {
	cur, err := s.CurrentSeq()
	if err != nil {
		return nil, nil, "", err
	}
	asOf = seqToken(cur)

	query := `SELECT t.id, t.kind, t.title, t.tags, t.creator, t.created_at, t.created_seq, t.last_activity_seq,
			rc.seq,
			EXISTS (SELECT 1 FROM mentions mn WHERE mn.thread_id = t.id AND mn.username = ? AND mn.seq > COALESCE(rc.seq, 0)) AS has_mention,
			(t.creator = ? OR EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = t.id AND m.author = ?)) AS is_participant
		 FROM threads t
		 LEFT JOIN read_cursors rc ON rc.thread_id = t.id AND rc.username = ?
		 WHERE t.last_activity_seq > COALESCE(rc.seq, 0)
		   AND (t.kind = 'public' OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = t.id AND p.username = ?))
		   AND (t.kind = 'dm'
		        OR t.creator = ?
		        OR EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = t.id AND m.author = ?)
		        OR EXISTS (SELECT 1 FROM mentions mn WHERE mn.thread_id = t.id AND mn.username = ? AND mn.seq > COALESCE(rc.seq, 0)))`
	args := []any{viewer, viewer, viewer, viewer, viewer, viewer, viewer, viewer}
	if before > 0 {
		query += ` AND t.last_activity_seq < ?`
		args = append(args, before)
	}
	query += ` ORDER BY t.last_activity_seq DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()

	items = []api.InboxItem{}
	for rows.Next() {
		var it api.InboxItem
		var tagsJSON string
		var createdSeq, lastActivitySeq int64
		var readSeq sql.NullInt64
		var hasMention, isParticipant int
		if err := rows.Scan(&it.Thread.ID, &it.Thread.Kind, &it.Thread.Title, &tagsJSON, &it.Thread.Creator,
			&it.Thread.CreatedAt, &createdSeq, &lastActivitySeq, &readSeq, &hasMention, &isParticipant); err != nil {
			return nil, nil, "", err
		}
		if err := json.Unmarshal([]byte(tagsJSON), &it.Thread.Tags); err != nil {
			return nil, nil, "", err
		}
		it.Thread.CreatedSeq = seqToken(createdSeq)
		it.Thread.LastActivitySeq = seqToken(lastActivitySeq)
		it.UpdatedSeq = it.Thread.LastActivitySeq
		if readSeq.Valid {
			tok := seqToken(readSeq.Int64)
			it.LastReadSeq = &tok
		}
		if hasMention != 0 {
			it.Reasons = append(it.Reasons, "mention")
		}
		if it.Thread.Kind == "dm" {
			it.Reasons = append(it.Reasons, "dm")
		} else if isParticipant != 0 {
			it.Reasons = append(it.Reasons, "participant")
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		tok := items[limit-1].Thread.LastActivitySeq
		nextPage = &tok
	}
	for i := range items {
		if items[i].Thread.Kind == "dm" {
			if items[i].Thread.Participants, err = loadParticipants(s.db, items[i].Thread.ID); err != nil {
				return nil, nil, "", err
			}
		}
	}
	return items, nextPage, asOf, nil
}

// GetReadCursor returns the viewer's read cursor for a visible thread, nil
// when never set.
func (s *Store) GetReadCursor(threadID, viewer string) (*int64, error) {
	if _, err := s.GetThread(threadID, viewer); err != nil {
		return nil, err
	}
	var seq int64
	err := s.db.QueryRow(`SELECT seq FROM read_cursors WHERE username = ? AND thread_id = ?`, viewer, threadID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &seq, nil
}

// SetReadCursor sets the viewer's read cursor for a visible thread to an
// absolute position — moving backward is allowed (marks things unread).
func (s *Store) SetReadCursor(threadID, viewer string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := getThread(tx, threadID, viewer); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO read_cursors (username, thread_id, seq) VALUES (?, ?, ?)
		 ON CONFLICT (username, thread_id) DO UPDATE SET seq = excluded.seq`,
		viewer, threadID, seq); err != nil {
		return err
	}
	return tx.Commit()
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
