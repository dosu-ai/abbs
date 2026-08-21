package store

import (
	"time"

	"github.com/dosu-ai/abbs/internal/api"
)

const userCols = `username, kind, display_name, owned_by, admin, deactivated, created_at`

func (s *Store) GetUser(username string) (api.User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

// ListUsers pages through all users alphabetically; after is the page
// anchor (last username of the previous page; empty for the first).
func (s *Store) ListUsers(after string, limit int) (items []api.User, nextPage *string, asOf string, err error) {
	cur, err := s.CurrentSeq()
	if err != nil {
		return nil, nil, "", err
	}
	asOf = seqToken(cur)
	rows, err := s.db.Query(`SELECT `+userCols+` FROM users WHERE username > ? ORDER BY username LIMIT ?`,
		after, limit+1)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()
	items = []api.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, nil, "", err
		}
		items = append(items, u)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		tok := items[limit-1].Username
		nextPage = &tok
	}
	return items, nextPage, asOf, nil
}

// DeactivateUser kills a user's credentials while keeping their records and
// attribution. Idempotent: deactivating a deactivated user returns the
// current state without a new event.
func (s *Store) DeactivateUser(username string, at time.Time) (api.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return api.User{}, err
	}
	defer tx.Rollback()

	u, err := scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if err != nil {
		return api.User{}, err
	}
	if u.Deactivated {
		return u, tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE users SET deactivated = 1 WHERE username = ?`, username); err != nil {
		return api.User{}, err
	}
	if _, err := insertEvent(tx, "user.deactivated", nil, now(at), map[string]any{"username": username}); err != nil {
		return api.User{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.User{}, err
	}
	u.Deactivated = true
	s.wakeup.notify()
	return u, nil
}

// SetAdmin grants or revokes the admin role — an operator action (abbs
// admin), deliberately not exposed over HTTP and orthogonal to auth mode.
func (s *Store) SetAdmin(username string, admin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE users SET admin = ? WHERE username = ?`, boolToInt(admin), username)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
