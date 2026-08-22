package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
)

const messageCols = `id, thread_id, author, content, mentions, deleted, created_at, edited_at, deleted_at, deleted_by, created_seq, seq`

// scanMessage scans the canonical message column list (messageCols),
// without reaction tallies.
func scanMessage(row rowScanner) (api.Message, error) {
	var m api.Message
	var content, mentions, editedAt, deletedAt, deletedBy sql.NullString
	var deleted int
	var createdSeq, seq int64
	err := row.Scan(&m.ID, &m.ThreadID, &m.Author, &content, &mentions, &deleted, &m.CreatedAt,
		&editedAt, &deletedAt, &deletedBy, &createdSeq, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, ErrNotFound
	}
	if err != nil {
		return api.Message{}, err
	}
	m.Deleted = deleted != 0
	if !m.Deleted {
		m.Content = content.String
		if mentions.Valid && mentions.String != "" {
			if err := json.Unmarshal([]byte(mentions.String), &m.Mentions); err != nil {
				return api.Message{}, err
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
	return m, nil
}

// GetMessage returns a message (tombstones included) if its thread is
// visible to the viewer.
func (s *Store) GetMessage(id, viewer string) (api.Message, error) {
	m, err := scanMessage(s.db.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id = ?`, id))
	if err != nil {
		return api.Message{}, err
	}
	if _, err := getThread(s.db, m.ThreadID, AuthenticatedViewer(viewer)); err != nil {
		return api.Message{}, err
	}
	if m.Reactions, err = tallies(s.db, m.ID); err != nil {
		return api.Message{}, err
	}
	return m, nil
}

// EditMessage replaces a message's content in place. Author-only; message
// IDs are stable across edits; the edit advances the thread's activity
// cursor and re-extracts mentions (a mention added by an edit reaches its
// target's inbox at the edit's seq).
func (s *Store) EditMessage(id, author, content string, at time.Time) (api.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()

	m, err := scanMessage(tx.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id = ?`, id))
	if err != nil {
		return api.Message{}, err
	}
	if _, err := getThread(tx, m.ThreadID, AuthenticatedViewer(author)); err != nil {
		return api.Message{}, err
	}
	if m.Deleted {
		return api.Message{}, ErrMessageDeleted
	}
	if m.Author != author {
		return api.Message{}, ErrForbidden
	}

	ts := now(at)
	seq, err := insertEvent(tx, "message.edited", &m.ThreadID, ts, nil)
	if err != nil {
		return api.Message{}, err
	}
	mentioned, err := extractMentions(tx, content)
	if err != nil {
		return api.Message{}, err
	}
	mentionsJSON, err := json.Marshal(mentioned)
	if err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(`DELETE FROM mentions WHERE message_id = ?`, id); err != nil {
		return api.Message{}, err
	}
	if err := insertMentions(tx, id, m.ThreadID, mentioned, seq); err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(
		`UPDATE messages SET content = ?, mentions = ?, edited_at = ?, seq = ? WHERE id = ?`,
		content, string(mentionsJSON), ts, seq, id); err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ?`, seq, m.ThreadID); err != nil {
		return api.Message{}, err
	}
	if err := advanceReadCursor(tx, author, m.ThreadID, seq); err != nil {
		return api.Message{}, err
	}

	m.Content = content
	m.Mentions = mentioned
	m.EditedAt = &ts
	m.Seq = seqToken(seq)
	if m.Reactions, err = tallies(tx, id); err != nil {
		return api.Message{}, err
	}
	if err := updateEventPayload(tx, seq, map[string]any{"message": m}); err != nil {
		return api.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, err
	}
	s.wakeup.notify()
	return m, nil
}

// DeleteMessage tombstones a message: id and position survive, content and
// mentions go. Author or admin only; deleted_by records who, so moderation
// is distinguishable from retraction. Idempotent — deleting a tombstone
// returns it unchanged without a new event. Admins may delete messages in
// threads they cannot read (moderation by id).
func (s *Store) DeleteMessage(id, actor string, isAdmin bool, at time.Time) (api.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()

	m, err := scanMessage(tx.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id = ?`, id))
	if err != nil {
		return api.Message{}, err
	}
	if !isAdmin {
		if _, err := getThread(tx, m.ThreadID, AuthenticatedViewer(actor)); err != nil {
			return api.Message{}, err
		}
		if m.Author != actor {
			return api.Message{}, ErrForbidden
		}
	}
	if m.Reactions, err = tallies(tx, id); err != nil {
		return api.Message{}, err
	}
	if m.Deleted {
		if err := tx.Commit(); err != nil {
			return api.Message{}, err
		}
		return m, nil
	}

	ts := now(at)
	seq, err := insertEvent(tx, "message.deleted", &m.ThreadID, ts, nil)
	if err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(`DELETE FROM mentions WHERE message_id = ?`, id); err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(
		`UPDATE messages SET content = NULL, mentions = NULL, deleted = 1, deleted_at = ?, deleted_by = ?, seq = ? WHERE id = ?`,
		ts, actor, seq, id); err != nil {
		return api.Message{}, err
	}
	if _, err := tx.Exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ?`, seq, m.ThreadID); err != nil {
		return api.Message{}, err
	}

	m.Deleted = true
	m.Content = ""
	m.Mentions = nil
	m.DeletedAt = ts
	m.DeletedBy = actor
	m.Seq = seqToken(seq)
	if err := updateEventPayload(tx, seq, map[string]any{"message": m}); err != nil {
		return api.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, err
	}
	s.wakeup.notify()
	return m, nil
}

// LastAuthors returns the authors of a thread's most recent n messages,
// newest first, with the created_at of the oldest returned message — the
// reply-loop guard's probe.
func (s *Store) LastAuthors(threadID string, n int) (authors []string, oldest time.Time, err error) {
	rows, err := s.db.Query(
		`SELECT author, created_at FROM messages WHERE thread_id = ? ORDER BY created_seq DESC LIMIT ?`,
		threadID, n)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var oldestStr string
	for rows.Next() {
		var author, createdAt string
		if err := rows.Scan(&author, &createdAt); err != nil {
			return nil, time.Time{}, err
		}
		authors = append(authors, author)
		oldestStr = createdAt
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	if oldestStr != "" {
		if oldest, err = time.Parse(time.RFC3339Nano, oldestStr); err != nil {
			return nil, time.Time{}, err
		}
	}
	return authors, oldest, nil
}
