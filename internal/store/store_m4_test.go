package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func lastEventType(t *testing.T, s *Store, viewer string) string {
	t.Helper()
	events, _, err := s.Events(viewer, 0, 100, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	return events[len(events)-1]["type"].(string)
}

func TestEditAndDeleteMessage(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	claim(t, s, "bob")
	claim(t, s, "mallory")

	thread, msg, err := s.CreateThread("alice", "editing", "first draft", nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// Non-author cannot edit.
	if _, err := s.EditMessage(msg.ID, "mallory", "hijacked", time.Now()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mallory edit: %v, want ErrForbidden", err)
	}

	// Edit replaces content in place, re-extracts mentions, advances the
	// thread's activity cursor, and bumps the message seq.
	edited, err := s.EditMessage(msg.ID, "alice", "second draft, hi @bob", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if edited.ID != msg.ID || edited.EditedAt == nil || edited.Seq == msg.Seq {
		t.Fatalf("edit result: %+v", edited)
	}
	if len(edited.Mentions) != 1 || edited.Mentions[0] != "bob" {
		t.Fatalf("edit mentions: %v", edited.Mentions)
	}
	after, err := s.GetThread(thread.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if after.LastActivitySeq != edited.Seq {
		t.Fatalf("edit did not advance activity: %s != %s", after.LastActivitySeq, edited.Seq)
	}
	if got := lastEventType(t, s, "alice"); got != "message.edited" {
		t.Fatalf("last event = %s", got)
	}
	// The mention added by the edit reaches bob's inbox.
	items, _, _, err := s.Inbox("bob", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Reasons[0] != "mention" {
		t.Fatalf("bob inbox after edit: %+v", items)
	}

	// Delete by non-author is forbidden; by author tombstones.
	if _, err := s.DeleteMessage(msg.ID, "mallory", false, time.Now()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mallory delete: %v, want ErrForbidden", err)
	}
	tomb, err := s.DeleteMessage(msg.ID, "alice", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !tomb.Deleted || tomb.Content != "" || tomb.Mentions != nil || tomb.DeletedBy != "alice" || tomb.DeletedAt == "" {
		t.Fatalf("tombstone: %+v", tomb)
	}
	// The tombstone clears bob's mention; he never posted, so the thread
	// leaves his inbox entirely.
	if items, _, _, _ := s.Inbox("bob", 0, 50); len(items) != 0 {
		t.Fatalf("bob inbox after delete: %+v", items)
	}
	// Idempotent: deleting again returns the tombstone, no new event.
	pre, _ := s.CurrentSeq()
	again, err := s.DeleteMessage(msg.ID, "alice", false, time.Now())
	if err != nil || !again.Deleted {
		t.Fatalf("re-delete: %+v, %v", again, err)
	}
	post, _ := s.CurrentSeq()
	if pre != post {
		t.Fatal("re-delete emitted an event")
	}
	// Editing a tombstone is refused.
	if _, err := s.EditMessage(msg.ID, "alice", "resurrect", time.Now()); !errors.Is(err, ErrMessageDeleted) {
		t.Fatalf("edit tombstone: %v, want ErrMessageDeleted", err)
	}
	// Tombstones stay in the list — pagination and cursors stay consistent.
	msgs, _, _, err := s.ListMessages(thread.ID, "alice", 0, 50)
	if err != nil || len(msgs) != 1 || !msgs[0].Deleted {
		t.Fatalf("list after delete: %+v, %v", msgs, err)
	}

	// Admin moderation delete on someone else's message records deleted_by.
	_, msg2, err := s.CreateThread("bob", "mod target", "spam", nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	modTomb, err := s.DeleteMessage(msg2.ID, "alice", true, time.Now())
	if err != nil || modTomb.DeletedBy != "alice" || modTomb.Author != "bob" {
		t.Fatalf("moderation delete: %+v, %v", modTomb, err)
	}
}

func TestReactionsStore(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	claim(t, s, "bob")

	thread, msg, err := s.CreateThread("alice", "votes", "proposal", nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// A reaction consumes the sequence but never bumps thread activity.
	if err := s.AddReaction(msg.ID, "bob", "👍", time.Now()); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetThread(thread.ID, "alice")
	if after.LastActivitySeq != thread.LastActivitySeq {
		t.Fatalf("reaction bumped thread activity: %s -> %s", thread.LastActivitySeq, after.LastActivitySeq)
	}
	if got := lastEventType(t, s, "alice"); got != "reaction.added" {
		t.Fatalf("last event = %s", got)
	}

	// It lands in the author's inbox as reason reaction, with updated_seq
	// past the thread's activity cursor.
	items, _, _, err := s.Inbox("alice", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Reasons) != 1 || items[0].Reasons[0] != "reaction" {
		t.Fatalf("alice inbox: %+v", items)
	}
	ur, _ := ParseSeq(items[0].UpdatedSeq)
	la, _ := ParseSeq(thread.LastActivitySeq)
	if ur <= la {
		t.Fatalf("updated_seq %d not past activity %d", ur, la)
	}
	// Marking read up to the reaction clears it.
	if err := s.SetReadCursor(thread.ID, "alice", ur); err != nil {
		t.Fatal(err)
	}
	if items, _, _, _ := s.Inbox("alice", 0, 50); len(items) != 0 {
		t.Fatalf("alice inbox after read: %+v", items)
	}

	// Idempotent add: no second event.
	pre, _ := s.CurrentSeq()
	if err := s.AddReaction(msg.ID, "bob", "👍", time.Now()); err != nil {
		t.Fatal(err)
	}
	if post, _ := s.CurrentSeq(); post != pre {
		t.Fatal("idempotent re-add emitted an event")
	}

	// Tallies and who-reacted list.
	if err := s.AddReaction(msg.ID, "alice", "👍", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddReaction(msg.ID, "bob", "🎉", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessage(msg.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reactions) != 2 || got.Reactions[0].Emoji != "👍" || got.Reactions[0].Count != 2 {
		t.Fatalf("tallies: %+v", got.Reactions)
	}
	rlist, _, _, err := s.ListReactions(msg.ID, "alice", 0, 50)
	if err != nil || len(rlist) != 3 {
		t.Fatalf("reaction list: %+v, %v", rlist, err)
	}

	// The 10-distinct-emoji cap per (user, message).
	emojis := []string{"😀", "😁", "😂", "😃", "😄", "😅", "😆", "😉"}
	for _, e := range emojis { // bob has 👍 + 🎉, these 8 reach the cap
		if err := s.AddReaction(msg.ID, "bob", e, time.Now()); err != nil {
			t.Fatalf("add %s: %v", e, err)
		}
	}
	if err := s.AddReaction(msg.ID, "bob", "💥", time.Now()); !errors.Is(err, ErrReactionLimit) {
		t.Fatalf("11th distinct emoji: %v, want ErrReactionLimit", err)
	}

	// Removal is idempotent and only touches own reactions.
	if err := s.RemoveReaction(msg.ID, "bob", "🎉", time.Now()); err != nil {
		t.Fatal(err)
	}
	pre, _ = s.CurrentSeq()
	if err := s.RemoveReaction(msg.ID, "bob", "🎉", time.Now()); err != nil {
		t.Fatal(err)
	}
	if post, _ := s.CurrentSeq(); post != pre {
		t.Fatal("idempotent re-remove emitted an event")
	}

	// Tombstones reject new reactions; existing ones survive and remain
	// removable.
	if _, err := s.DeleteMessage(msg.ID, "alice", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddReaction(msg.ID, "bob", "💥", time.Now()); !errors.Is(err, ErrMessageDeleted) {
		t.Fatalf("react to tombstone: %v, want ErrMessageDeleted", err)
	}
	tomb, _ := s.GetMessage(msg.ID, "alice")
	if len(tomb.Reactions) == 0 {
		t.Fatal("reactions did not survive the tombstone")
	}
	if err := s.RemoveReaction(msg.ID, "bob", "👍", time.Now()); err != nil {
		t.Fatalf("remove own reaction from tombstone: %v", err)
	}
}

func TestTagsAndSubscriptions(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	claim(t, s, "bob")
	claim(t, s, "carol")

	thread, _, err := s.CreateThread("alice", "tagging", "content", []string{"old"}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// A stranger (never posted) cannot retag a public thread.
	if _, err := s.UpdateThreadTags(thread.ID, "carol", []string{"hijack"}, time.Now()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger retag: %v, want ErrForbidden", err)
	}
	// A poster can.
	if _, err := s.PostMessage(thread.ID, "bob", "chiming in", time.Now()); err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateThreadTags(thread.ID, "bob", []string{"new", "shared"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Tags) != 2 || updated.Tags[0] != "new" {
		t.Fatalf("tags after update: %v", updated.Tags)
	}
	// The tag change advanced the activity cursor and emitted an event.
	if updated.LastActivitySeq == thread.LastActivitySeq {
		t.Fatal("tag change did not advance activity cursor")
	}
	if got := lastEventType(t, s, "alice"); got != "thread.tags_changed" {
		t.Fatalf("last event = %s", got)
	}

	// DM tags are invisible to outsiders in the tag listing.
	if _, _, err := s.CreateThread("alice", "private", "x", []string{"secret", "shared"}, []string{"alice", "bob"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	tags, _, _, err := s.ListTags("carol", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].Name != "new" || tags[1].Name != "shared" || tags[1].ThreadCount != 1 {
		t.Fatalf("carol tags: %+v", tags)
	}
	// bob (a DM participant) sees: new(1), secret(1), shared(2).
	bobTags, _, _, _ := s.ListTags("bob", "", 50)
	if len(bobTags) != 3 || bobTags[2].Name != "shared" || bobTags[2].ThreadCount != 2 {
		t.Fatalf("bob tags: %+v", bobTags)
	}

	// Subscriptions round-trip, idempotently.
	if err := s.SubscribeTag("carol", "new"); err != nil {
		t.Fatal(err)
	}
	if err := s.SubscribeTag("carol", "new"); err != nil {
		t.Fatal(err)
	}
	subs, _, _, err := s.ListTagSubscriptions("carol", "", 50)
	if err != nil || len(subs) != 1 || subs[0] != "new" {
		t.Fatalf("subs: %v, %v", subs, err)
	}
	// Subscribed-tags event filter narrows to matching threads.
	events, _, err := s.Events("carol", 0, 100, EventFilter{SubscribedTags: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if tid, ok := ev["thread_id"]; ok && tid != thread.ID {
			t.Fatalf("subscribed filter leaked event: %v", ev)
		}
	}
	if len(events) == 0 {
		t.Fatal("subscribed filter returned nothing")
	}
	if err := s.UnsubscribeTag("carol", "new"); err != nil {
		t.Fatal(err)
	}
	if subs, _, _, _ := s.ListTagSubscriptions("carol", "", 50); len(subs) != 0 {
		t.Fatalf("subs after unsubscribe: %v", subs)
	}
}

func TestEventFilters(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	claim(t, s, "bob")

	pub, _, err := s.CreateThread("alice", "public", "mentioning @bob here", []string{"go"}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dm, _, err := s.CreateThread("alice", "dm", "private", nil, []string{"alice", "bob"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// mentions filter: only the event carrying bob's mention.
	events, _, err := s.Events("bob", 0, 100, EventFilter{Mentions: true})
	if err != nil || len(events) != 1 || events[0]["type"] != "message.created" {
		t.Fatalf("mentions filter: %v, %v", events, err)
	}
	// dms filter: only DM-thread events.
	events, _, err = s.Events("bob", 0, 100, EventFilter{DMs: true})
	if err != nil || len(events) != 2 {
		t.Fatalf("dms filter: %v, %v", events, err)
	}
	for _, ev := range events {
		if th, ok := ev["thread"].(map[string]any); ok && th["id"] != dm.ID {
			t.Fatalf("dms filter leaked: %v", ev)
		}
	}
	// tag filter.
	events, _, err = s.Events("bob", 0, 100, EventFilter{Tags: []string{"go"}})
	if err != nil || len(events) != 2 {
		t.Fatalf("tag filter: %v, %v", events, err)
	}
	// Union: mentions OR dms.
	events, _, err = s.Events("bob", 0, 100, EventFilter{Mentions: true, DMs: true})
	if err != nil || len(events) != 3 {
		t.Fatalf("union filter: %v, %v", events, err)
	}
	_ = pub
}

func TestUsersAndDeactivation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "abbs.db"))
	defer s.Close()
	claim(t, s, "alice")
	claim(t, s, "bob")
	claim(t, s, "carol")

	users, next, _, err := s.ListUsers("", 2)
	if err != nil || len(users) != 2 || next == nil {
		t.Fatalf("page1: %v, %v, %v", users, next, err)
	}
	users2, next2, _, err := s.ListUsers(*next, 2)
	if err != nil || len(users2) != 1 || next2 != nil || users2[0].Username != "carol" {
		t.Fatalf("page2: %v, %v, %v", users2, next2, err)
	}

	if err := s.SetAdmin("alice", true); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUser("alice")
	if err != nil || !u.Admin {
		t.Fatalf("admin flag: %+v, %v", u, err)
	}
	if err := s.SetAdmin("ghost", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant to ghost: %v", err)
	}

	// Deactivation kills credentials, keeps the record, emits the event —
	// idempotently.
	deactivated, err := s.DeactivateUser("bob", time.Now())
	if err != nil || !deactivated.Deactivated {
		t.Fatalf("deactivate: %+v, %v", deactivated, err)
	}
	if _, err := s.UserByTokenHash("hash-bob"); err != nil {
		t.Fatalf("deactivated user record gone: %v", err) // record survives; the server layer rejects
	}
	if got := lastEventType(t, s, "alice"); got != "user.deactivated" {
		t.Fatalf("last event = %s", got)
	}
	pre, _ := s.CurrentSeq()
	if _, err := s.DeactivateUser("bob", time.Now()); err != nil {
		t.Fatal(err)
	}
	if post, _ := s.CurrentSeq(); post != pre {
		t.Fatal("re-deactivation emitted an event")
	}
}
