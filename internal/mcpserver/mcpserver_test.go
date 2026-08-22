package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
)

// end-to-end: MCP client → in-memory transport → adapter → real HTTP →
// real server → real SQLite. The M3 exit criterion in test form.
func TestToolsEndToEnd(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st, server.Config{WorkspaceName: "dogfood"}))
	t.Cleanup(func() { ts.Close(); st.Close() })

	anon := &client.Client{BaseURL: ts.URL}
	aliceClaim, err := anon.ClaimUser(ctx, api.ClaimUserRequest{Username: "alice", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	bobClaim, err := anon.ClaimUser(ctx, api.ClaimUserRequest{Username: "bob", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}

	connect := func(token string) *mcp.ClientSession {
		srv := New([]*Workspace{{Name: "dogfood", Label: "dogfood", URL: ts.URL, Client: &client.Client{BaseURL: ts.URL, Token: token}}})
		clientT, serverT := mcp.NewInMemoryTransports()
		go srv.Run(ctx, serverT)
		session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		return session
	}
	alice := connect(aliceClaim.Token)
	bob := connect(bobClaim.Token)

	call := func(s *mcp.ClientSession, tool string, args map[string]any, out any) {
		t.Helper()
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if res.IsError {
			t.Fatalf("%s returned tool error: %+v", tool, res.Content)
		}
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decode structured content: %v", tool, err)
		}
	}

	// Alice creates a thread mentioning bob.
	var created threadOut
	call(alice, "create_thread", map[string]any{
		"title": "standup", "content": "morning @bob — status?", "tags": []string{"Standup"},
	}, &created)
	if created.Workspace != "dogfood" || created.Thread.Tags[0] != "standup" {
		t.Fatalf("create_thread = %+v", created)
	}

	// Bob's inbox has it, reason mention.
	var ib inboxOut
	call(bob, "inbox", map[string]any{}, &ib)
	if len(ib.Items) != 1 || ib.Items[0].Reasons[0] != "mention" || ib.Items[0].Thread.ID != created.Thread.ID {
		t.Fatalf("bob inbox = %+v", ib)
	}

	// Bob reads the thread and replies.
	var rt readThreadOut
	call(bob, "read_thread", map[string]any{"thread_id": created.Thread.ID}, &rt)
	if len(rt.Messages) != 1 || rt.Messages[0].Author != "alice" || rt.Messages[0].Mentions[0] != "bob" {
		t.Fatalf("read_thread = %+v", rt)
	}
	var rep replyOut
	call(bob, "reply", map[string]any{"thread_id": created.Thread.ID, "content": "all green"}, &rep)

	// Bob marks read; his inbox clears. Alice's now shows the reply.
	var mr markReadOut
	call(bob, "mark_read", map[string]any{"thread_id": created.Thread.ID, "seq": rep.Message.Seq}, &mr)
	call(bob, "inbox", map[string]any{}, &ib)
	if len(ib.Items) != 0 {
		t.Fatalf("bob inbox after mark_read = %+v", ib.Items)
	}
	call(alice, "inbox", map[string]any{}, &ib)
	if len(ib.Items) != 1 || ib.Items[0].Reasons[0] != "participant" {
		t.Fatalf("alice inbox = %+v", ib.Items)
	}

	// list_threads with tag filter.
	var lt listThreadsOut
	call(alice, "list_threads", map[string]any{"tags": []string{"standup"}}, &lt)
	if len(lt.Items) != 1 || lt.Items[0].ID != created.Thread.ID || lt.Workspace != "dogfood" {
		t.Fatalf("list_threads = %+v", lt)
	}

	// A tool error surfaces as an MCP tool error, not a protocol failure.
	res, err := bob.CallTool(ctx, &mcp.CallToolParams{Name: "read_thread", Arguments: map[string]any{"thread_id": "00000000-0000-0000-0000-000000000000"}})
	if err != nil {
		t.Fatalf("protocol error on missing thread: %v", err)
	}
	if !res.IsError {
		t.Fatal("read_thread of a missing thread should be a tool error")
	}
}
