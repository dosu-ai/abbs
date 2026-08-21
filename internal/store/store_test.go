package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
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

func TestMentionsAndInbox(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	claim(t, s, "bob")
	claim(t, s, "carol")

	// Mentions resolve only to existing users; trailing punctuation is
	// handled; emails are not mentions.
	thread, first, err := s.CreateThread("alice", "mentions", "ping @bob. also @ghost and mail@carol.example", nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Mentions) != 1 || first.Mentions[0] != "bob" {
		t.Fatalf("mentions = %v, want [bob]", first.Mentions)
	}

	// Bob is mentioned → inbox with reason mention; not a participant yet.
	items, _, _, err := s.Inbox("bob", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Thread.ID != thread.ID {
		t.Fatalf("bob inbox = %+v, want the mention thread", items)
	}
	if len(items[0].Reasons) != 1 || items[0].Reasons[0] != "mention" {
		t.Fatalf("bob reasons = %v, want [mention]", items[0].Reasons)
	}
	if items[0].LastReadSeq != nil {
		t.Fatalf("bob has a read cursor he never set: %v", *items[0].LastReadSeq)
	}

	// Carol: not mentioned, not a participant → empty inbox.
	if items, _, _, _ := s.Inbox("carol", 0, 50); len(items) != 0 {
		t.Fatalf("carol inbox = %+v, want empty", items)
	}

	// Alice posted it herself → her own thread is read, inbox empty.
	if items, _, _, _ := s.Inbox("alice", 0, 50); len(items) != 0 {
		t.Fatalf("alice inbox = %+v, want empty (own post auto-reads)", items)
	}

	// Bob reads up to date → inbox clears.
	last, _ := ParseSeq(thread.LastActivitySeq)
	if err := s.SetReadCursor(thread.ID, "bob", last); err != nil {
		t.Fatal(err)
	}
	if items, _, _, _ := s.Inbox("bob", 0, 50); len(items) != 0 {
		t.Fatalf("bob inbox after mark-read = %+v, want empty", items)
	}
	got, err := s.GetReadCursor(thread.ID, "bob")
	if err != nil || got == nil || *got != last {
		t.Fatalf("read cursor = %v, %v; want %d", got, err, last)
	}

	// Bob replies → alice's inbox lights up with reason participant (she
	// created it), and bob's own reply doesn't reappear in his inbox.
	if _, err := s.PostMessage(thread.ID, "bob", "pong", time.Now()); err != nil {
		t.Fatal(err)
	}
	items, _, _, err = s.Inbox("alice", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Reasons[0] != "participant" {
		t.Fatalf("alice inbox = %+v, want [participant]", items)
	}
	if items, _, _, _ := s.Inbox("bob", 0, 50); len(items) != 0 {
		t.Fatalf("bob inbox after his own reply = %+v, want empty", items)
	}

	// DM inbox: reason dm for the non-poster; invisible to outsiders even
	// when they are mentioned inside it.
	dm, _, err := s.CreateThread("alice", "secret", "psst @bob, do not tell @carol", nil, []string{"alice", "bob"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	items, _, _, _ = s.Inbox("bob", 0, 50)
	var dmItem *api.InboxItem
	for i := range items {
		if items[i].Thread.ID == dm.ID {
			dmItem = &items[i]
		}
	}
	if dmItem == nil {
		t.Fatalf("bob inbox lacks the DM: %+v", items)
	}
	want := []string{"mention", "dm"}
	if len(dmItem.Reasons) != 2 || dmItem.Reasons[0] != want[0] || dmItem.Reasons[1] != want[1] {
		t.Fatalf("dm reasons = %v, want %v", dmItem.Reasons, want)
	}
	if items, _, _, _ := s.Inbox("carol", 0, 50); len(items) != 0 {
		t.Fatalf("carol inbox leaks a DM she was mentioned in: %+v", items)
	}
}

func TestListThreads(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	claim(t, s, "bob")
	claim(t, s, "carol")

	a, _, err := s.CreateThread("alice", "go talk", "x", []string{"go", "dev"}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.CreateThread("alice", "cooking", "y", []string{"food"}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dm, _, err := s.CreateThread("alice", "private", "z", nil, []string{"alice", "bob"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// Carol sees the two public threads, newest activity first, no DM.
	items, _, _, err := s.ListThreads("carol", 0, 0, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != b.ID || items[1].ID != a.ID {
		t.Fatalf("carol threads = %+v", items)
	}
	// Bob sees the DM too, with participants populated.
	items, _, _, _ = s.ListThreads("bob", 0, 0, nil, 50)
	if len(items) != 3 || items[0].ID != dm.ID || len(items[0].Participants) != 2 {
		t.Fatalf("bob threads = %+v", items)
	}
	// Tag filter is any-of.
	items, _, _, _ = s.ListThreads("carol", 0, 0, []string{"go", "food"}, 50)
	if len(items) != 2 {
		t.Fatalf("tag any-of = %+v", items)
	}
	items, _, _, _ = s.ListThreads("carol", 0, 0, []string{"dev"}, 50)
	if len(items) != 1 || items[0].ID != a.ID {
		t.Fatalf("tag dev = %+v", items)
	}
	// since: only threads with activity after a's creation burst.
	aSeq, _ := ParseSeq(a.LastActivitySeq)
	items, _, _, _ = s.ListThreads("carol", aSeq, 0, nil, 50)
	if len(items) != 1 || items[0].ID != b.ID {
		t.Fatalf("since = %+v", items)
	}
	// Pagination walks without overlap.
	page1, next, _, _ := s.ListThreads("bob", 0, 0, nil, 2)
	if next == nil || len(page1) != 2 {
		t.Fatalf("page1 = %+v next = %v", page1, next)
	}
	anchor, _ := ParseSeq(*next)
	page2, next2, _, _ := s.ListThreads("bob", 0, anchor, nil, 2)
	if next2 != nil || len(page2) != 1 || page2[0].ID == page1[0].ID || page2[0].ID == page1[1].ID {
		t.Fatalf("page2 = %+v next = %v", page2, next2)
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
