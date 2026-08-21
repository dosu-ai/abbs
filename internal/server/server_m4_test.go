package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/store"
)

func newServerCfg(t *testing.T, cfg Config) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceName == "" {
		cfg.WorkspaceName = "testspace"
	}
	ts := httptest.NewServer(New(st, cfg))
	t.Cleanup(func() { ts.Close(); st.Close() })
	return ts, st
}

// doRaw sends a request with optional Idempotency-Key and returns status,
// headers, and body.
func (c *client) doRaw(method, path, idemKey string, body any) (int, http.Header, []byte) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, c.base+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp.StatusCode, resp.Header, out.Bytes()
}

func TestIdempotencyKeys(t *testing.T) {
	ts, _ := newServerCfg(t, Config{})
	alice := claim(t, ts.URL, "alice")

	body := api.CreateThreadRequest{Title: "once", Content: "exactly once"}
	status1, _, resp1 := alice.doRaw("POST", "/v1/threads", "key-1", body)
	if status1 != http.StatusCreated {
		t.Fatalf("first create = %d: %s", status1, resp1)
	}
	// Identical replay returns the original response — same thread id, no
	// duplicate thread.
	status2, _, resp2 := alice.doRaw("POST", "/v1/threads", "key-1", body)
	if status2 != http.StatusCreated || !bytes.Equal(resp1, resp2) {
		t.Fatalf("replay: %d, bodies equal=%v", status2, bytes.Equal(resp1, resp2))
	}
	var page api.ThreadPage
	alice.do("GET", "/v1/threads", nil, http.StatusOK, &page)
	if len(page.Items) != 1 {
		t.Fatalf("replay duplicated the thread: %d threads", len(page.Items))
	}
	// Same key, different body → conflict, never a silent replay.
	status3, _, resp3 := alice.doRaw("POST", "/v1/threads", "key-1", api.CreateThreadRequest{Title: "different", Content: "body"})
	if status3 != http.StatusConflict || !strings.Contains(string(resp3), "idempotency-key-conflict") {
		t.Fatalf("conflict: %d %s", status3, resp3)
	}
	// Keys are per-endpoint: the same key on another endpoint is fresh.
	var thread api.Thread
	json.Unmarshal(resp1, &thread)
	status4, _, _ := alice.doRaw("POST", "/v1/threads/"+thread.ID+"/messages", "key-1", api.CreateMessageRequest{Content: "reply"})
	if status4 != http.StatusCreated {
		t.Fatalf("per-endpoint scope: %d", status4)
	}
	// Keys are per-principal: bob may reuse alice's key.
	bob := claim(t, ts.URL, "bob")
	status5, _, _ := bob.doRaw("POST", "/v1/threads", "key-1", body)
	if status5 != http.StatusCreated {
		t.Fatalf("per-principal scope: %d", status5)
	}
}

func TestRateLimit(t *testing.T) {
	ts, _ := newServerCfg(t, Config{WriteBurst: 3, WriteRefillPerSec: 0.01})
	// The claim itself is charged to the pre-auth "claim:alice" principal,
	// so alice's own bucket starts full at 3.
	alice := claim(t, ts.URL, "alice")
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "a", Content: "x"}, http.StatusCreated, nil)
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "b", Content: "x"}, http.StatusCreated, nil)
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "c", Content: "x"}, http.StatusCreated, nil)
	status, headers, body := alice.doRaw("POST", "/v1/threads", "", api.CreateThreadRequest{Title: "d", Content: "x"})
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "rate-limited") {
		t.Fatalf("rate limit: %d %s", status, body)
	}
	if headers.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	// Reads are not limited.
	alice.do("GET", "/v1/threads", nil, http.StatusOK, nil)
}

func TestLoopGuard(t *testing.T) {
	ts, _ := newServerCfg(t, Config{LoopGuardMessages: 3, LoopGuardWindow: time.Minute})
	alice := claim(t, ts.URL, "alice")
	bob := claim(t, ts.URL, "bob")
	carol := claim(t, ts.URL, "carol")

	var thread api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "loop", Content: "ping"}, http.StatusCreated, &thread)
	bob.do("POST", "/v1/threads/"+thread.ID+"/messages", api.CreateMessageRequest{Content: "pong"}, http.StatusCreated, nil)
	alice.do("POST", "/v1/threads/"+thread.ID+"/messages", api.CreateMessageRequest{Content: "ping"}, http.StatusCreated, nil)

	// Last 3 messages: alice, bob, alice — bob's next reply makes 2
	// distinct authors over the threshold inside the window.
	status, headers, body := bob.doRaw("POST", "/v1/threads/"+thread.ID+"/messages", "", api.CreateMessageRequest{Content: "pong"})
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "loop-guard") {
		t.Fatalf("loop guard: %d %s", status, body)
	}
	if headers.Get("Retry-After") == "" {
		t.Fatal("loop-guard 429 without Retry-After")
	}
	// A third voice breaks the loop shape.
	carol.do("POST", "/v1/threads/"+thread.ID+"/messages", api.CreateMessageRequest{Content: "you two ok?"}, http.StatusCreated, nil)
	bob.do("POST", "/v1/threads/"+thread.ID+"/messages", api.CreateMessageRequest{Content: "thanks carol"}, http.StatusCreated, nil)
}

func TestFullSurfaceHTTP(t *testing.T) {
	ts, st := newServerCfg(t, Config{})
	alice := claim(t, ts.URL, "alice")
	bob := claim(t, ts.URL, "bob")

	var thread api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "surface", Content: "v1", Tags: []string{"api"}}, http.StatusCreated, &thread)
	var msgs api.MessagePage
	alice.do("GET", "/v1/threads/"+thread.ID+"/messages", nil, http.StatusOK, &msgs)
	msgID := msgs.Items[0].ID

	// Edit: author-only, 200 with edited_at.
	bob.do("PATCH", "/v1/messages/"+msgID, api.EditMessageRequest{Content: "hijack"}, http.StatusForbidden, nil)
	var edited api.Message
	alice.do("PATCH", "/v1/messages/"+msgID, api.EditMessageRequest{Content: "v2"}, http.StatusOK, &edited)
	if edited.EditedAt == nil || edited.Content != "v2" {
		t.Fatalf("edit: %+v", edited)
	}

	// Reactions: PUT/DELETE with URL-encoded emoji, tallies on the message.
	emojiPath := "/v1/messages/" + msgID + "/reactions/" + url.PathEscape("👍🏽")
	if status, _, _ := bob.doRaw("PUT", emojiPath, "", nil); status != http.StatusNoContent {
		t.Fatalf("add reaction: %d", status)
	}
	if status, _, body := bob.doRaw("PUT", "/v1/messages/"+msgID+"/reactions/x", "", nil); status != http.StatusUnprocessableEntity || !strings.Contains(string(body), "invalid-emoji") {
		t.Fatalf("invalid emoji: %d %s", status, body)
	}
	var got api.Message
	alice.do("GET", "/v1/messages/"+msgID, nil, http.StatusOK, &got)
	if len(got.Reactions) != 1 || got.Reactions[0].Emoji != "👍🏽" || got.Reactions[0].Count != 1 {
		t.Fatalf("tallies: %+v", got.Reactions)
	}
	var rpage api.ReactionPage
	alice.do("GET", "/v1/messages/"+msgID+"/reactions", nil, http.StatusOK, &rpage)
	if len(rpage.Items) != 1 || rpage.Items[0].Username != "bob" {
		t.Fatalf("reaction list: %+v", rpage.Items)
	}

	// Alice's inbox got the reaction reason.
	var inbox api.InboxPage
	alice.do("GET", "/v1/inbox", nil, http.StatusOK, &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0].Reasons[0] != "reaction" {
		t.Fatalf("inbox: %+v", inbox.Items)
	}

	// Tags: PATCH replaces; listing and subscriptions round-trip.
	var retagged api.Thread
	alice.do("PATCH", "/v1/threads/"+thread.ID, api.UpdateThreadRequest{Tags: []string{"API", "design "}}, http.StatusOK, &retagged)
	if len(retagged.Tags) != 2 || retagged.Tags[0] != "api" || retagged.Tags[1] != "design" {
		t.Fatalf("retag: %v", retagged.Tags)
	}
	var tags api.TagPage
	alice.do("GET", "/v1/tags", nil, http.StatusOK, &tags)
	if len(tags.Items) != 2 {
		t.Fatalf("tags: %+v", tags.Items)
	}
	if status, _, _ := bob.doRaw("PUT", "/v1/tag-subscriptions/design", "", nil); status != http.StatusNoContent {
		t.Fatal("subscribe failed")
	}
	var subs api.TagSubscriptionPage
	bob.do("GET", "/v1/tag-subscriptions", nil, http.StatusOK, &subs)
	if len(subs.Items) != 1 || subs.Items[0] != "design" {
		t.Fatalf("subs: %+v", subs.Items)
	}
	// Filtered long-poll: subscribed_tags narrows to this thread's events.
	var batch api.EventBatch
	bob.do("GET", "/v1/events?subscribed_tags=true", nil, http.StatusOK, &batch)
	if len(batch.Events) == 0 {
		t.Fatal("subscribed_tags poll returned nothing")
	}

	// Users list/get.
	var users api.UserPage
	alice.do("GET", "/v1/users?limit=1", nil, http.StatusOK, &users)
	if len(users.Items) != 1 || users.NextPage == nil {
		t.Fatalf("users page: %+v", users)
	}
	var u api.User
	alice.do("GET", "/v1/users/bob", nil, http.StatusOK, &u)

	// Admin: non-admin cannot deactivate or moderate; admin can. The role
	// is granted operator-side (abbs admin → store.SetAdmin).
	alice.do("POST", "/v1/users/bob/deactivate", nil, http.StatusForbidden, nil)
	if err := st.SetAdmin("alice", true); err != nil {
		t.Fatal(err)
	}
	var reply api.Message
	bob.do("POST", "/v1/threads/"+thread.ID+"/messages", api.CreateMessageRequest{Content: "to be moderated"}, http.StatusCreated, &reply)
	var tomb api.Message
	alice.do("DELETE", "/v1/messages/"+reply.ID, nil, http.StatusOK, &tomb)
	if !tomb.Deleted || tomb.DeletedBy != "alice" || tomb.Author != "bob" {
		t.Fatalf("moderation tombstone: %+v", tomb)
	}
	// React to the tombstone → 409 message-deleted.
	if status, _, body := bob.doRaw("PUT", "/v1/messages/"+reply.ID+"/reactions/"+url.PathEscape("👍"), "", nil); status != http.StatusConflict || !strings.Contains(string(body), "message-deleted") {
		t.Fatalf("react to tombstone: %d %s", status, body)
	}

	var deactivated api.User
	alice.do("POST", "/v1/users/bob/deactivate", nil, http.StatusOK, &deactivated)
	if !deactivated.Deactivated {
		t.Fatalf("deactivate: %+v", deactivated)
	}
	// Bob's credentials are dead; his records survive.
	bob.do("GET", "/v1/inbox", nil, http.StatusUnauthorized, nil)
	alice.do("GET", "/v1/users/bob", nil, http.StatusOK, &u)
	if !u.Deactivated {
		t.Fatalf("bob's record: %+v", u)
	}
}
