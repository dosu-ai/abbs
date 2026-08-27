package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/store"
)

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body any, status int, out any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			c.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		var p bytes.Buffer
		p.ReadFrom(resp.Body)
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, status, p.String())
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatal(err)
		}
	}
}

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(MustNew(st, Config{WorkspaceName: "testspace"}))
	t.Cleanup(func() { ts.Close(); st.Close() })
	return ts, st
}

func claim(t *testing.T, base, username string) *client {
	t.Helper()
	c := &client{t: t, base: base}
	var resp api.ClaimUserResponse
	c.do("POST", "/v1/users", api.ClaimUserRequest{Username: username, Kind: "agent"}, http.StatusCreated, &resp)
	c.token = resp.Token
	return c
}

func TestGetCurrentUser(t *testing.T) {
	ts, st := newServer(t)
	alice := claim(t, ts.URL, "alice")
	if err := st.SetAdmin("alice", true); err != nil {
		t.Fatal(err)
	}

	var current api.User
	alice.do("GET", "/v1/me", nil, http.StatusOK, &current)
	if current.Username != "alice" || current.Kind != "agent" || !current.Admin || current.Deactivated || current.CreatedAt == "" {
		t.Fatalf("current user: %+v", current)
	}

	(&client{t: t, base: ts.URL}).do("GET", "/v1/me", nil, http.StatusUnauthorized, nil)
	(&client{t: t, base: ts.URL, token: "unknown"}).do("GET", "/v1/me", nil, http.StatusUnauthorized, nil)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic malformed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("malformed credential = %d, want 401", resp.StatusCode)
	}

	const replacementToken = "abbs_replacement"
	if err := st.RotateToken("alice", hashToken(replacementToken)); err != nil {
		t.Fatal(err)
	}
	alice.do("GET", "/v1/me", nil, http.StatusUnauthorized, nil)
	replacement := &client{t: t, base: ts.URL, token: replacementToken}
	replacement.do("GET", "/v1/me", nil, http.StatusOK, &current)
	if _, err := st.DeactivateUser("alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	replacement.do("GET", "/v1/me", nil, http.StatusUnauthorized, nil)
}

// TestConversation is the M2 exit criterion over HTTP: two principals hold
// a conversation through the API.
func TestConversation(t *testing.T) {
	ts, _ := newServer(t)
	alice := claim(t, ts.URL, "alice")
	bob := claim(t, ts.URL, "bob")

	var info api.ServerInfo
	alice.do("GET", "/v1/server", nil, http.StatusOK, &info)
	if info.Workspace.Name != "testspace" || info.AuthModes[0] != "first-claim" {
		t.Fatalf("server info: %+v", info)
	}

	var thread api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{
		Title: "hello", Content: "hi @bob", Tags: []string{" Intro ", "intro"},
	}, http.StatusCreated, &thread)
	if len(thread.Tags) != 1 || thread.Tags[0] != "intro" {
		t.Fatalf("tags not normalized: %v", thread.Tags)
	}

	// Bob catches up from cursor 0, finds the thread, replies.
	var batch api.EventBatch
	bob.do("GET", "/v1/events", nil, http.StatusOK, &batch)
	var threadID string
	for _, ev := range batch.Events {
		if ev["type"] == "thread.created" {
			threadID = ev["thread"].(map[string]any)["id"].(string)
		}
	}
	if threadID != thread.ID {
		t.Fatalf("bob's catch-up found thread %q, want %q", threadID, thread.ID)
	}
	var reply api.Message
	bob.do("POST", "/v1/threads/"+threadID+"/messages", api.CreateMessageRequest{Content: "hi alice"}, http.StatusCreated, &reply)

	// Alice reads the thread: both messages, in order.
	var page api.MessagePage
	alice.do("GET", "/v1/threads/"+threadID+"/messages", nil, http.StatusOK, &page)
	if len(page.Items) != 2 || page.Items[0].Author != "alice" || page.Items[1].Author != "bob" {
		t.Fatalf("thread read: %+v", page.Items)
	}
	if page.AsOf == "" {
		t.Error("page missing as_of (snapshot-then-tail anchor)")
	}

	// Alice resumes from her cursor and sees only bob's reply.
	var tail api.EventBatch
	alice.do("GET", "/v1/events?cursor="+thread.LastActivitySeq, nil, http.StatusOK, &tail)
	if len(tail.Events) != 1 || tail.Events[0]["seq"] != reply.Seq {
		t.Fatalf("tail: %v", tail.Events)
	}

	// Caught up: empty batch echoes the cursor.
	var echo api.EventBatch
	alice.do("GET", "/v1/events?cursor="+tail.Cursor, nil, http.StatusOK, &echo)
	if len(echo.Events) != 0 || echo.Cursor != tail.Cursor {
		t.Fatalf("echo: %+v", echo)
	}
}

// TestLongPollWakeup: a poll blocked on timeout returns early when an event
// arrives via the in-process broadcast.
func TestLongPollWakeup(t *testing.T) {
	ts, _ := newServer(t)
	alice := claim(t, ts.URL, "alice")
	bob := claim(t, ts.URL, "bob")

	var cur api.EventBatch
	bob.do("GET", "/v1/events", nil, http.StatusOK, &cur)

	done := make(chan api.EventBatch, 1)
	go func() {
		var batch api.EventBatch
		bob.do("GET", "/v1/events?cursor="+cur.Cursor+"&timeout=30", nil, http.StatusOK, &batch)
		done <- batch
	}()

	time.Sleep(150 * time.Millisecond) // let the poll park
	start := time.Now()
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "wake up", Content: "ping"}, http.StatusCreated, nil)

	select {
	case batch := <-done:
		if len(batch.Events) == 0 {
			t.Fatal("woken poll returned no events")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("poll took %v after the event; wakeup is broken", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long-poll never returned after an event")
	}
}

func TestErrors(t *testing.T) {
	ts, _ := newServer(t)
	alice := claim(t, ts.URL, "alice")
	claim(t, ts.URL, "bob") // exists as a DM participant below

	// Duplicate claim → 409 username-taken.
	anon := &client{t: t, base: ts.URL}
	var problem api.Problem
	alice.do("POST", "/v1/users", api.ClaimUserRequest{Username: "alice", Kind: "human"}, http.StatusConflict, &problem)
	if !strings.HasSuffix(problem.Type, "/username-taken") {
		t.Errorf("problem type = %s", problem.Type)
	}

	// Bad username → 400.
	alice.do("POST", "/v1/users", api.ClaimUserRequest{Username: "Not Valid!", Kind: "agent"}, http.StatusBadRequest, nil)

	// No token → 401 problem+json.
	anon.do("GET", "/v1/events", nil, http.StatusUnauthorized, &problem)

	// Over-limit content → 422 content-too-long, distinct from validation.
	var thread api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "limits", Content: "ok"}, http.StatusCreated, &thread)
	long := strings.Repeat("x", 8001)
	alice.do("POST", "/v1/threads/"+thread.ID+"/messages", api.CreateMessageRequest{Content: long}, http.StatusUnprocessableEntity, &problem)
	if !strings.HasSuffix(problem.Type, "/content-too-long") {
		t.Errorf("problem type = %s", problem.Type)
	}
	// Exactly 8000 code points is fine — and it is code points, not bytes.
	exact := strings.Repeat("é", 8000)
	alice.do("POST", "/v1/threads/"+thread.ID+"/messages", api.CreateMessageRequest{Content: exact}, http.StatusCreated, nil)

	// DM invisible to outsiders → 404, and unknown participant → 400.
	var dm api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "private", Content: "psst", Participants: []string{"bob"}}, http.StatusCreated, &dm)
	if dm.Kind != "dm" || len(dm.Participants) != 2 {
		t.Fatalf("dm shape: %+v", dm)
	}
	carol := claim(t, ts.URL, "carol")
	carol.do("GET", "/v1/threads/"+dm.ID, nil, http.StatusNotFound, nil)
	carol.do("POST", "/v1/threads/"+dm.ID+"/messages", api.CreateMessageRequest{Content: "hi"}, http.StatusNotFound, nil)
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "ghost", Content: "x", Participants: []string{"nobody"}}, http.StatusBadRequest, nil)

	// Unknown route → problem+json 404.
	alice.do("GET", "/v1/definitely-not-a-thing", nil, http.StatusNotFound, &problem)

	// Bad cursor / timeout → 400.
	alice.do("GET", "/v1/events?cursor=banana", nil, http.StatusBadRequest, nil)
	alice.do("GET", "/v1/events?timeout=999", nil, http.StatusBadRequest, nil)
}
