package cache

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
)

// harness: a real server + two claimed users, and a cache for alice.
type harness struct {
	ts    *httptest.Server
	alice *client.Client
	bob   *client.Client
	dir   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.MustNew(st, server.Config{WorkspaceName: "test"}))
	t.Cleanup(func() { ts.Close(); st.Close() })

	anon := &client.Client{BaseURL: ts.URL}
	h := &harness{ts: ts, dir: t.TempDir()}
	for name, dst := range map[string]**client.Client{"alice": &h.alice, "bob": &h.bob} {
		resp, err := anon.ClaimUser(ctx, api.ClaimUserRequest{Username: name, Kind: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		*dst = &client.Client{BaseURL: ts.URL, Token: resp.Token}
	}
	return h
}

func (h *harness) openCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(h.dir, "alice.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// catchUp drains all events past the cache's cursor into it (timeout 0 —
// the catch-up read and the long-poll are the same query).
func catchUp(t *testing.T, c *Cache, cl *client.Client) {
	t.Helper()
	ctx := context.Background()
	for {
		cursor, _, err := c.Cursor()
		if err != nil {
			t.Fatal(err)
		}
		batch, err := cl.Events(ctx, client.EventsOptions{Cursor: cursor, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Apply(batch); err != nil {
			t.Fatal(err)
		}
		if len(batch.Events) == 0 {
			return
		}
	}
}

// assertMatchesServer compares the cache's reads to the server's for
// alice's visible slice.
func assertMatchesServer(t *testing.T, c *Cache, cl *client.Client) {
	t.Helper()
	ctx := context.Background()
	serverThreads, err := cl.ListThreads(ctx, client.ListThreadsOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	cacheThreads, err := c.ListThreads(ListThreadsOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serverThreads.Items, cacheThreads.Items) {
		t.Fatalf("threads diverge:\nserver: %+v\ncache:  %+v", serverThreads.Items, cacheThreads.Items)
	}
	for _, th := range serverThreads.Items {
		sm, err := cl.ListMessages(ctx, th.ID, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		cm, err := c.ListMessages(th.ID, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sm.Items, cm.Items) {
			t.Fatalf("messages diverge in %s:\nserver: %+v\ncache:  %+v", th.ID, sm.Items, cm.Items)
		}
	}
}

// TestSnapshotThenTailStitch is the M7 stitch test: bootstrap from the
// paginated read endpoints, keep writing, tail, and end bit-identical to
// the server — including writes that landed between snapshot and tail.
func TestSnapshotThenTailStitch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	th1, err := h.alice.CreateThread(ctx, api.CreateThreadRequest{Title: "before", Content: "hello @bob", Tags: []string{"m7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.bob.PostMessage(ctx, th1.ID, "hi back"); err != nil {
		t.Fatal(err)
	}

	c := h.openCache(t)
	if err := c.Bootstrap(ctx, h.alice); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Cursor(); !ok {
		t.Fatal("bootstrap left no cursor")
	}

	// Writes after the snapshot: a new thread, a reply, an edit-shaped
	// tombstone via delete is covered elsewhere; here activity + tags.
	th2, err := h.bob.CreateThread(ctx, api.CreateThreadRequest{Title: "after", Content: "post-snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.alice.PostMessage(ctx, th2.ID, "tailed in"); err != nil {
		t.Fatal(err)
	}

	catchUp(t, c, h.alice)
	assertMatchesServer(t, c, h.alice)

	// Overlap tolerance: re-applying the whole log from cursor 0 must be a
	// no-op — application is idempotent by construction.
	batch, err := h.alice.Events(ctx, client.EventsOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(batch); err != nil {
		t.Fatal(err)
	}
	catchUp(t, c, h.alice)
	assertMatchesServer(t, c, h.alice)
}

// TestRebuildAfterDelete: deleting the cache file at any time rebuilds
// cleanly (the M7 exit criterion).
func TestRebuildAfterDelete(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	th, err := h.alice.CreateThread(ctx, api.CreateThreadRequest{Title: "t", Content: "c"})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(h.dir, "alice.db")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Bootstrap(ctx, h.alice); err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}

	c2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, ok, _ := c2.Cursor(); ok {
		t.Fatal("fresh file should have no cursor")
	}
	sy := &Syncer{Cache: c2, Client: h.alice}
	if err := sy.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.bob.PostMessage(ctx, th.ID, "after rebuild"); err != nil {
		t.Fatal(err)
	}
	catchUp(t, c2, h.alice)
	assertMatchesServer(t, c2, h.alice)
}

// TestEvolutionRules retires the M5-deferred debt: unknown event types and
// unknown fields on known types must neither crash the cache loop nor
// stall its cursor.
func TestEvolutionRules(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	batch := api.EventBatch{
		Cursor: "42",
		Events: []api.Event{
			// Unknown type entirely.
			{"seq": "40", "type": "quantum.flux", "occurred_at": "2026-01-01T00:00:00Z", "amplitude": 0.7},
			// Known type with unknown extra fields.
			{"seq": "41", "type": "thread.created", "occurred_at": "2026-01-01T00:00:00Z",
				"thread": map[string]any{
					"id": "11111111-1111-7111-8111-111111111111", "kind": "public", "title": "t",
					"tags": []string{}, "creator": "alice", "created_at": "2026-01-01T00:00:00Z",
					"created_seq": "41", "last_activity_seq": "41",
					"future_field": "ignored",
				},
				"another_future_field": true},
			// Known type with a malformed payload: skipped, never fatal.
			{"seq": "42", "type": "message.created", "occurred_at": "2026-01-01T00:00:00Z",
				"message": "not an object"},
		},
	}
	if err := c.Apply(batch); err != nil {
		t.Fatalf("evolution-rule batch must not fail: %v", err)
	}
	cursor, ok, err := c.Cursor()
	if err != nil || !ok || cursor != "42" {
		t.Fatalf("cursor must advance past unknown events: got %q ok=%v err=%v", cursor, ok, err)
	}
	tp, err := c.ListThreads(ListThreadsOptions{})
	if err != nil || len(tp.Items) != 1 || tp.Items[0].Title != "t" {
		t.Fatalf("known event alongside unknown ones must still apply: %+v err=%v", tp.Items, err)
	}
}

// TestReactionsIdempotent: per-(message,user,emoji) rows make replayed
// reaction events harmless, and tallies serve from the cache.
func TestReactionsIdempotent(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mid := "22222222-2222-7222-8222-222222222222"
	base := api.EventBatch{Cursor: "2", Events: []api.Event{
		{"seq": "1", "type": "thread.created", "occurred_at": "x", "thread": map[string]any{
			"id": "t1", "kind": "public", "title": "t", "tags": []string{}, "creator": "a",
			"created_at": "x", "created_seq": "1", "last_activity_seq": "2"}},
		{"seq": "2", "type": "message.created", "occurred_at": "x", "message": map[string]any{
			"id": mid, "thread_id": "t1", "author": "a", "content": "hi", "deleted": false,
			"created_at": "x", "seq": "2", "reactions": []any{}}},
	}}
	react := api.EventBatch{Cursor: "3", Events: []api.Event{
		{"seq": "3", "type": "reaction.added", "occurred_at": "x",
			"thread_id": "t1", "message_id": mid, "emoji": "👍", "username": "bob"},
	}}
	for _, b := range []api.EventBatch{base, react, react, base} { // replays included
		if err := c.Apply(b); err != nil {
			t.Fatal(err)
		}
	}
	mp, err := c.ListMessages("t1", "", 0)
	if err != nil || len(mp.Items) != 1 {
		t.Fatalf("messages: %+v err=%v", mp.Items, err)
	}
	if len(mp.Items[0].Reactions) != 1 || mp.Items[0].Reactions[0].Count != 1 {
		t.Fatalf("tally must be exactly one 👍: %+v", mp.Items[0].Reactions)
	}

	removed := api.EventBatch{Cursor: "4", Events: []api.Event{
		{"seq": "4", "type": "reaction.removed", "occurred_at": "x",
			"thread_id": "t1", "message_id": mid, "emoji": "👍", "username": "bob"},
	}}
	if err := c.Apply(removed); err != nil {
		t.Fatal(err)
	}
	mp, _ = c.ListMessages("t1", "", 0)
	if len(mp.Items[0].Reactions) != 0 {
		t.Fatalf("tally after removal: %+v", mp.Items[0].Reactions)
	}
}

// TestEditsAndTombstones: edits and deletes are full-state upserts.
func TestEditsAndTombstones(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	th, err := h.alice.CreateThread(ctx, api.CreateThreadRequest{Title: "t", Content: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := h.bob.PostMessage(ctx, th.ID, "bob's message")
	if err != nil {
		t.Fatal(err)
	}

	c := h.openCache(t)
	if err := c.Bootstrap(ctx, h.alice); err != nil {
		t.Fatal(err)
	}

	// Edit and delete after the snapshot, tail them in.
	if _, err := h.bob.EditMessage(ctx, msg.ID, "bob's edit"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.bob.DeleteMessage(ctx, msg.ID); err != nil {
		t.Fatal(err)
	}
	catchUp(t, c, h.alice)
	assertMatchesServer(t, c, h.alice)

	mp, err := c.ListMessages(th.ID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range mp.Items {
		if m.ID == msg.ID {
			found = true
			if !m.Deleted || m.Content != "" {
				t.Fatalf("expected tombstone, got %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("tombstone must survive in pagination")
	}
}
