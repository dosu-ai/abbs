package cache

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dosu-ai/abbs/internal/api"
)

// Read methods mirror the server's list semantics so the MCP adapter can
// serve from the cache with the same shapes. Page tokens are opaque to
// callers; as_of is the cache's replay cursor.

// ErrNotFound is returned when the id is not (or not yet) in the cache.
var ErrNotFound = sql.ErrNoRows

func (c *Cache) asOf() string {
	cursor, _, _ := c.Cursor()
	return cursor
}

type ListThreadsOptions struct {
	Since string
	Tags  []string
	Page  string
	Limit int
}

func (c *Cache) ListThreads(opts ListThreadsOptions) (api.ThreadPage, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query := `SELECT id, kind, title, creator, tags, participants, created_at, created_seq, last_activity_seq
		FROM threads WHERE 1=1`
	var args []any
	if opts.Since != "" {
		query += ` AND last_activity_seq > ?`
		args = append(args, seqInt(opts.Since))
	}
	if opts.Page != "" {
		query += ` AND last_activity_seq < ?`
		args = append(args, seqInt(opts.Page))
	}
	if len(opts.Tags) > 0 {
		clause := `EXISTS (SELECT 1 FROM json_each(threads.tags) WHERE json_each.value = ?)`
		query += ` AND (` + clause + strings.Repeat(` OR `+clause, len(opts.Tags)-1) + `)`
		for _, t := range opts.Tags {
			args = append(args, t)
		}
	}
	query += ` ORDER BY last_activity_seq DESC LIMIT ?`
	args = append(args, limit+1)

	// asOf before Query: the pool is one connection, and a second query
	// while rows are open would self-deadlock.
	page := api.ThreadPage{Items: []api.Thread{}, AsOf: c.asOf()}
	rows, err := c.db.Query(query, args...)
	if err != nil {
		return api.ThreadPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return api.ThreadPage{}, err
		}
		page.Items = append(page.Items, t)
	}
	if err := rows.Err(); err != nil {
		return api.ThreadPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		tok := page.Items[limit-1].LastActivitySeq
		page.NextPage = &tok
	}
	return page, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanThread(r rowScanner) (api.Thread, error) {
	var t api.Thread
	var tags, participants string
	var createdSeq, activitySeq int64
	if err := r.Scan(&t.ID, &t.Kind, &t.Title, &t.Creator, &tags, &participants, &t.CreatedAt, &createdSeq, &activitySeq); err != nil {
		return t, err
	}
	json.Unmarshal([]byte(tags), &t.Tags)
	json.Unmarshal([]byte(participants), &t.Participants)
	// The wire omits empty participants (omitempty); mirror that so cache
	// reads are byte-identical to server reads.
	if len(t.Participants) == 0 {
		t.Participants = nil
	}
	if t.Tags == nil {
		t.Tags = []string{}
	}
	t.CreatedSeq = strconv.FormatInt(createdSeq, 10)
	t.LastActivitySeq = strconv.FormatInt(activitySeq, 10)
	return t, nil
}

func (c *Cache) GetThread(id string) (api.Thread, error) {
	row := c.db.QueryRow(`SELECT id, kind, title, creator, tags, participants, created_at, created_seq, last_activity_seq
		FROM threads WHERE id = ?`, id)
	return scanThread(row)
}

func (c *Cache) ListMessages(threadID, page string, limit int) (api.MessagePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query := `SELECT id, thread_id, author, content, mentions, deleted, created_at, edited_at, deleted_at, deleted_by, seq
		FROM messages WHERE thread_id = ?`
	args := []any{threadID}
	if page != "" {
		query += ` AND seq > ?`
		args = append(args, seqInt(page))
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit+1)

	// asOf before Query — see ListThreads.
	mp := api.MessagePage{Items: []api.Message{}, AsOf: c.asOf()}
	rows, err := c.db.Query(query, args...)
	if err != nil {
		return api.MessagePage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var m api.Message
		var mentions string
		var edited sql.NullString
		var deleted int
		var seq int64
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Author, &m.Content, &mentions, &deleted, &m.CreatedAt, &edited, &m.DeletedAt, &m.DeletedBy, &seq); err != nil {
			return api.MessagePage{}, err
		}
		json.Unmarshal([]byte(mentions), &m.Mentions)
		if len(m.Mentions) == 0 {
			m.Mentions = nil // omitempty on the wire
		}
		m.Deleted = deleted != 0
		if edited.Valid {
			m.EditedAt = &edited.String
		}
		m.Seq = strconv.FormatInt(seq, 10)
		mp.Items = append(mp.Items, m)
	}
	if err := rows.Err(); err != nil {
		return api.MessagePage{}, err
	}
	more := len(mp.Items) > limit
	if more {
		mp.Items = mp.Items[:limit]
	}
	for i := range mp.Items {
		tally, err := c.tally(mp.Items[i].ID)
		if err != nil {
			return api.MessagePage{}, err
		}
		mp.Items[i].Reactions = tally
	}
	if more {
		tok := mp.Items[limit-1].Seq
		mp.NextPage = &tok
	}
	return mp, nil
}

func (c *Cache) tally(messageID string) ([]api.ReactionTally, error) {
	rows, err := c.db.Query(`SELECT emoji, count(*) FROM reactions WHERE message_id = ? GROUP BY emoji ORDER BY count(*) DESC, emoji`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tally := []api.ReactionTally{}
	for rows.Next() {
		var t api.ReactionTally
		if err := rows.Scan(&t.Emoji, &t.Count); err != nil {
			return nil, err
		}
		tally = append(tally, t)
	}
	return tally, rows.Err()
}
