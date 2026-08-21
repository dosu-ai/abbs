package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRotateToken(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "bot")

	if _, err := s.UserByTokenHash("hash-bot"); err != nil {
		t.Fatalf("original credential: %v", err)
	}
	if err := s.RotateToken("bot", "hash-bot-2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// The old credential is revoked immediately; the new one resolves.
	if _, err := s.UserByTokenHash("hash-bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old credential still resolves: %v", err)
	}
	u, err := s.UserByTokenHash("hash-bot-2")
	if err != nil || u.Username != "bot" {
		t.Fatalf("new credential: %v %+v", err, u)
	}
	if err := s.RotateToken("nobody", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate unknown user: %v", err)
	}
}
