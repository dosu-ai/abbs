package store

import (
	"database/sql"
	"errors"
	"time"
)

// IdemRecord is a remembered write: enough to detect body mismatches and
// replay the original response byte-for-byte.
type IdemRecord struct {
	RequestHash string
	Status      int
	ContentType string
	Body        []byte
}

// IdemGet looks up a remembered (principal, endpoint, key) write no older
// than the retention horizon. nil means no fresh record.
func (s *Store) IdemGet(principal, endpoint, key string, notBefore time.Time) (*IdemRecord, error) {
	var rec IdemRecord
	err := s.db.QueryRow(
		`SELECT request_hash, status, content_type, body FROM idempotency
		 WHERE principal = ? AND endpoint = ? AND key = ? AND created_ns >= ?`,
		principal, endpoint, key, notBefore.UnixNano(),
	).Scan(&rec.RequestHash, &rec.Status, &rec.ContentType, &rec.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// IdemPut remembers a completed write and lazily purges expired records.
func (s *Store) IdemPut(principal, endpoint, key string, rec IdemRecord, at, purgeBefore time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM idempotency WHERE created_ns < ?`, purgeBefore.UnixNano()); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO idempotency (principal, endpoint, key, request_hash, status, content_type, body, created_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		principal, endpoint, key, rec.RequestHash, rec.Status, rec.ContentType, rec.Body, at.UnixNano())
	return err
}
