package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func claim(t *testing.T, s *Store, username string) {
	t.Helper()
	if _, err := s.ClaimUser(username, "agent", nil, "hash-"+username, time.Now()); err != nil {
		t.Fatalf("claim %s: %v", username, err)
	}
}

func TestConversationAndCursors(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()

	claim(t, s, "alice")
	claim(t, s, "bob")
	if _, err := s.ClaimUser("alice", "human", nil, "other-hash", time.Now()); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("reclaim: want ErrUsernameTaken, got %v", err)
	}

	thread, first, err := s.CreateThread("alice", "hello", "hi bob", []string{"intro"}, nil, time.Now())
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.LastActivitySeq != first.Seq {
		t.Errorf("thread activity %s != first message seq %s", thread.LastActivitySeq, first.Seq)
	}
	reply, err := s.PostMessage(thread.ID, "bob", "hi alice", time.Now())
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	events, cursor, err := s.Events("bob", 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	wantTypes := []string{"user.created", "user.created", "thread.created", "message.created", "message.created"}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i]["type"] != want {
			t.Errorf("event %d type = %v, want %s", i, events[i]["type"], want)
		}
	}
	if cursor != reply.Seq {
		t.Errorf("cursor %s != last seq %s", cursor, reply.Seq)
	}

	// Resume from a mid-stream cursor: only later events return.
	mid, _ := ParseSeq(first.Seq)
	tail, _, err := s.Events("bob", mid, 100)
	if err != nil {
		t.Fatalf("tail events: %v", err)
	}
	if len(tail) != 1 || tail[0]["type"] != "message.created" {
		t.Fatalf("tail = %v, want the single reply event", tail)
	}

	// Empty batch echoes the cursor.
	last, _ := ParseSeq(cursor)
	none, echo, err := s.Events("bob", last, 100)
	if err != nil || len(none) != 0 || echo != cursor {
		t.Fatalf("echo: events=%v cursor=%s err=%v, want empty batch echoing %s", none, echo, err, cursor)
	}
}

func TestDMVisibility(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()

	claim(t, s, "alice")
	claim(t, s, "bob")
	claim(t, s, "carol")

	dm, _, err := s.CreateThread("alice", "private", "between us", nil, []string{"alice", "bob"}, time.Now())
	if err != nil {
		t.Fatalf("create dm: %v", err)
	}

	if _, err := s.GetThread(dm.ID, "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("carol sees the DM: %v", err)
	}
	if _, err := s.GetThread(dm.ID, "bob"); err != nil {
		t.Errorf("bob cannot see the DM: %v", err)
	}
	if _, err := s.PostMessage(dm.ID, "carol", "let me in", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("carol posted to the DM: %v", err)
	}

	carolEvents, _, err := s.Events("carol", 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range carolEvents {
		if ev["type"] == "thread.created" || ev["type"] == "message.created" {
			t.Errorf("carol's stream leaks a DM event: %v", ev)
		}
	}
	bobEvents, _, err := s.Events("bob", 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	sawDM := false
	for _, ev := range bobEvents {
		if ev["type"] == "thread.created" {
			sawDM = true
		}
	}
	if !sawDM {
		t.Error("bob's stream is missing the DM thread.created")
	}

	if _, _, err := s.CreateThread("alice", "ghost dm", "hello?", nil, []string{"alice", "nobody"}, time.Now()); err == nil {
		t.Error("DM with unknown participant succeeded")
	}
}

// TestDurabilityAcrossReopen simulates the kill-9-and-restart exit
// criterion at the storage layer: close, reopen, nothing lost, sequence
// still monotonic, cursors resume.
func TestDurabilityAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abbs.db")
	s := open(t, path)
	claim(t, s, "alice")
	thread, _, err := s.CreateThread("alice", "persistent", "before restart", nil, nil, time.Now())
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	_, preCursor, err := s.Events("alice", 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := open(t, path)
	defer s2.Close()
	pre, _ := ParseSeq(preCursor)
	msg, err := s2.PostMessage(thread.ID, "alice", "after restart", time.Now())
	if err != nil {
		t.Fatalf("post after reopen: %v", err)
	}
	post, _ := ParseSeq(msg.Seq)
	if post <= pre {
		t.Fatalf("sequence went backwards across restart: %d <= %d", post, pre)
	}
	tail, _, err := s2.Events("alice", pre, 100)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(tail) != 1 || tail[0]["seq"] != msg.Seq {
		t.Fatalf("resuming from pre-restart cursor: got %v, want just the new message", tail)
	}
	msgs, _, _, err := s2.ListMessages(thread.ID, "alice", 0, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages after restart = %d, want 2 (nothing lost)", len(msgs))
	}
}

func TestMessagePagination(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	thread, _, err := s.CreateThread("alice", "pages", "msg 0", nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 5; i++ {
		if _, err := s.PostMessage(thread.ID, "alice", "more", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	after := int64(0)
	for {
		items, next, _, err := s.ListMessages(thread.ID, "alice", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		got += len(items)
		if next == nil {
			break
		}
		after, _ = ParseSeq(*next)
	}
	if got != 5 {
		t.Fatalf("paged through %d messages, want 5", got)
	}
}
