package store

import (
	"encoding/json"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
)

// UpdateThreadTags replaces a thread's tag set. Permitted for the creator
// and participants — in a DM, the fixed participant set (visibility already
// guarantees membership); in a public thread, the creator or anyone who has
// posted. Advances the activity cursor and emits thread.tags_changed.
// Tags must already be normalized.
func (s *Store) UpdateThreadTags(threadID, actor string, tags []string, at time.Time) (api.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return api.Thread{}, err
	}
	defer tx.Rollback()

	t, err := getThread(tx, threadID, AuthenticatedViewer(actor))
	if err != nil {
		return api.Thread{}, err
	}
	if t.Kind == "public" && t.Creator != actor {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM messages WHERE thread_id = ? AND author = ? LIMIT 1`,
			threadID, actor).Scan(&one); err != nil {
			return api.Thread{}, ErrForbidden
		}
	}

	if tags == nil {
		tags = []string{}
	}
	ts := now(at)
	seq, err := insertEvent(tx, "thread.tags_changed", &threadID, ts, map[string]any{
		"thread_id": threadID, "tags": tags, "actor": actor,
	})
	if err != nil {
		return api.Thread{}, err
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return api.Thread{}, err
	}
	if _, err := tx.Exec(`UPDATE threads SET tags = ?, last_activity_seq = ? WHERE id = ?`, string(tagsJSON), seq, threadID); err != nil {
		return api.Thread{}, err
	}
	if _, err := tx.Exec(`DELETE FROM thread_tags WHERE thread_id = ?`, threadID); err != nil {
		return api.Thread{}, err
	}
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO thread_tags (thread_id, tag) VALUES (?, ?)`, threadID, tag); err != nil {
			return api.Thread{}, err
		}
	}
	if err := advanceReadCursor(tx, actor, threadID, seq); err != nil {
		return api.Thread{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Thread{}, err
	}
	s.wakeup.notify()

	t.Tags = tags
	t.LastActivitySeq = seqToken(seq)
	return t, nil
}

// ListTags pages through tags on at least one thread visible to the viewer,
// with usage counts, alphabetically. after is the page anchor (last tag of
// the previous page; empty for the first).
func (s *Store) ListTags(viewer ReadViewer, after string, limit int) (items []api.TagInfo, nextPage *string, asOf string, err error) {
	cur, err := s.CurrentSeq()
	if err != nil {
		return nil, nil, "", err
	}
	asOf = seqToken(cur)

	query := `SELECT tt.tag, COUNT(*) FROM thread_tags tt JOIN threads t ON t.id = tt.thread_id
		 WHERE tt.tag > ? AND `
	args := []any{after}
	if viewer.authenticated {
		query += `(t.kind = 'public' OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = t.id AND p.username = ?))`
		args = append(args, viewer.username)
	} else {
		query += `t.kind = 'public'`
	}
	query += ` GROUP BY tt.tag ORDER BY tt.tag LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()
	items = []api.TagInfo{}
	for rows.Next() {
		var ti api.TagInfo
		if err := rows.Scan(&ti.Name, &ti.ThreadCount); err != nil {
			return nil, nil, "", err
		}
		items = append(items, ti)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		tok := items[limit-1].Name
		nextPage = &tok
	}
	return items, nextPage, asOf, nil
}

// SubscribeTag is idempotent; the tag need not be in use yet. Subscriptions
// are per-user state, not workspace events.
func (s *Store) SubscribeTag(username, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO tag_subscriptions (username, tag) VALUES (?, ?)`, username, tag)
	return err
}

func (s *Store) UnsubscribeTag(username, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM tag_subscriptions WHERE username = ? AND tag = ?`, username, tag)
	return err
}

func (s *Store) ListTagSubscriptions(username, after string, limit int) (items []string, nextPage *string, asOf string, err error) {
	cur, err := s.CurrentSeq()
	if err != nil {
		return nil, nil, "", err
	}
	asOf = seqToken(cur)
	rows, err := s.db.Query(
		`SELECT tag FROM tag_subscriptions WHERE username = ? AND tag > ? ORDER BY tag LIMIT ?`,
		username, after, limit+1)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()
	items = []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, nil, "", err
		}
		items = append(items, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		tok := items[limit-1]
		nextPage = &tok
	}
	return items, nextPage, asOf, nil
}
