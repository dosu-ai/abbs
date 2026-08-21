package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
)

// AddReaction records (viewer, message, emoji) idempotently: re-adding an
// existing reaction succeeds without a new event. At most 10 distinct emoji
// per user per message; tombstones reject new reactions. The emoji must
// already be normalized (emoji.Normalize). Reaction events consume the
// global sequence but deliberately never touch the thread's activity
// cursor.
func (s *Store) AddReaction(messageID, viewer, emojiKey string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	m, err := scanMessage(tx.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id = ?`, messageID))
	if err != nil {
		return err
	}
	if _, err := getThread(tx, m.ThreadID, viewer); err != nil {
		return err
	}
	if m.Deleted {
		return ErrMessageDeleted
	}

	var one int
	err = tx.QueryRow(`SELECT 1 FROM reactions WHERE message_id = ? AND username = ? AND emoji = ?`,
		messageID, viewer, emojiKey).Scan(&one)
	if err == nil {
		return nil // idempotent: already present, no event
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var distinct int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = ? AND username = ?`,
		messageID, viewer).Scan(&distinct); err != nil {
		return err
	}
	if distinct >= 10 {
		return ErrReactionLimit
	}

	ts := now(at)
	seq, err := insertEvent(tx, "reaction.added", &m.ThreadID, ts, map[string]any{
		"thread_id": m.ThreadID, "message_id": messageID, "emoji": emojiKey, "username": viewer,
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO reactions (message_id, username, emoji, created_at, seq) VALUES (?, ?, ?, ?, ?)`,
		messageID, viewer, emojiKey, ts, seq); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeup.notify()
	return nil
}

// RemoveReaction removes the viewer's own reaction, idempotently: removing
// an absent reaction succeeds without a new event. Reactions on tombstones
// survive but may still be removed by their reactor.
func (s *Store) RemoveReaction(messageID, viewer, emojiKey string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	m, err := scanMessage(tx.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id = ?`, messageID))
	if err != nil {
		return err
	}
	if _, err := getThread(tx, m.ThreadID, viewer); err != nil {
		return err
	}

	res, err := tx.Exec(`DELETE FROM reactions WHERE message_id = ? AND username = ? AND emoji = ?`,
		messageID, viewer, emojiKey)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil // idempotent: nothing to remove, no event
	}
	if _, err := insertEvent(tx, "reaction.removed", &m.ThreadID, now(at), map[string]any{
		"thread_id": m.ThreadID, "message_id": messageID, "emoji": emojiKey, "username": viewer,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeup.notify()
	return nil
}

// ListReactions pages through who-reacted-what in creation order; after is
// the seq page anchor.
func (s *Store) ListReactions(messageID, viewer string, after int64, limit int) (items []api.Reaction, nextPage *string, asOf string, err error) {
	if _, err := s.GetMessage(messageID, viewer); err != nil {
		return nil, nil, "", err
	}
	cur, err := s.CurrentSeq()
	if err != nil {
		return nil, nil, "", err
	}
	asOf = seqToken(cur)

	rows, err := s.db.Query(
		`SELECT emoji, username, created_at, seq FROM reactions WHERE message_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
		messageID, after, limit+1)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()
	items = []api.Reaction{}
	seqs := []int64{}
	for rows.Next() {
		var r api.Reaction
		var seq int64
		if err := rows.Scan(&r.Emoji, &r.Username, &r.CreatedAt, &seq); err != nil {
			return nil, nil, "", err
		}
		items = append(items, r)
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		tok := seqToken(seqs[limit-1])
		nextPage = &tok
	}
	return items, nextPage, asOf, nil
}
