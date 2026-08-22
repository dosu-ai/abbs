package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/cache"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
)

// Two real workspaces (two servers), one adapter: the M7 exit-criterion
// demo in test form — cached reads, workspace routing, merged inbox, and
// the read_only posture.
func TestMultiWorkspace(t *testing.T) {
	ctx := context.Background()

	boot := func(name string) (*httptest.Server, *client.Client, *client.Client) {
		st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(server.MustNew(st, server.Config{WorkspaceName: name}))
		t.Cleanup(func() { ts.Close(); st.Close() })
		anon := &client.Client{BaseURL: ts.URL}
		me, err := anon.ClaimUser(ctx, api.ClaimUserRequest{Username: "me", Kind: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		other, err := anon.ClaimUser(ctx, api.ClaimUserRequest{Username: "other", Kind: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		return ts, &client.Client{BaseURL: ts.URL, Token: me.Token}, &client.Client{BaseURL: ts.URL, Token: other.Token}
	}
	localTS, localMe, localOther := boot("local")
	sharedTS, sharedMe, sharedOther := boot("shared")

	// Seed both workspaces with a mention of me.
	if _, err := localOther.CreateThread(ctx, api.CreateThreadRequest{Title: "local ping", Content: "hey @me", Tags: []string{"m7"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sharedOther.CreateThread(ctx, api.CreateThreadRequest{Title: "shared ping", Content: "yo @me"}); err != nil {
		t.Fatal(err)
	}

	newWS := func(name, url string, cl *client.Client, readOnly bool) *Workspace {
		ca, err := cache.Open(filepath.Join(t.TempDir(), name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ca.Close() })
		sy := &cache.Syncer{Cache: ca, Client: cl}
		if err := sy.Ensure(ctx); err != nil {
			t.Fatal(err)
		}
		return &Workspace{Name: name, Label: name, URL: url, Client: cl, Cache: ca, ReadOnly: readOnly}
	}
	local := newWS("local", localTS.URL, localMe, false)
	shared := newWS("shared", sharedTS.URL, sharedMe, true) // read-only posture

	srv := New([]*Workspace{local, shared})
	clientT, serverT := mcp.NewInMemoryTransports()
	go srv.Run(ctx, serverT)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	call := func(tool string, args map[string]any, out any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if out != nil && !res.IsError {
			b, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatalf("%s: decode: %v", tool, err)
			}
		}
		return res
	}

	// list_workspaces names both, with postures.
	var lw listWorkspacesOut
	if res := call("list_workspaces", map[string]any{}, &lw); res.IsError {
		t.Fatalf("list_workspaces error: %+v", res.Content)
	}
	if len(lw.Workspaces) != 2 || lw.Workspaces[0].Name != "local" || lw.Workspaces[1].Name != "shared" || !lw.Workspaces[1].ReadOnly {
		t.Fatalf("list_workspaces = %+v", lw.Workspaces)
	}

	// Omitting workspace with two configured is a type error for reads.
	if res := call("list_threads", map[string]any{}, nil); !res.IsError {
		t.Fatal("ambiguous workspace must be a tool error")
	}

	// Cached reads route per workspace and carry the label.
	var lt listThreadsOut
	call("list_threads", map[string]any{"workspace": "local", "tags": []string{"m7"}}, &lt)
	if lt.Workspace != "local" || len(lt.Items) != 1 || lt.Items[0].Title != "local ping" {
		t.Fatalf("local list_threads = %+v", lt)
	}
	var rt readThreadOut
	call("read_thread", map[string]any{"workspace": "local", "thread_id": lt.Items[0].ID}, &rt)
	if len(rt.Messages) != 1 || rt.Messages[0].Content != "hey @me" {
		t.Fatalf("cached read_thread = %+v", rt)
	}

	// Merged inbox: one item from each workspace, labeled.
	var ib inboxOut
	call("inbox", map[string]any{}, &ib)
	if len(ib.Items) != 2 {
		t.Fatalf("merged inbox = %+v", ib.Items)
	}
	seen := map[string]string{}
	for _, item := range ib.Items {
		seen[item.Workspace] = item.Thread.Title
	}
	if seen["local"] != "local ping" || seen["shared"] != "shared ping" {
		t.Fatalf("merged inbox labels = %v", seen)
	}

	// read_only posture refuses every write tool on the shared workspace.
	for tool, args := range map[string]map[string]any{
		"create_thread": {"workspace": "shared", "title": "x", "content": "y"},
		"reply":         {"workspace": "shared", "thread_id": lt.Items[0].ID, "content": "y"},
		"mark_read":     {"workspace": "shared", "thread_id": lt.Items[0].ID, "seq": "1"},
	} {
		res := call(tool, args, nil)
		if !res.IsError {
			t.Fatalf("%s must be refused on a read-only workspace", tool)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if !strings.Contains(text, "read-only") {
			t.Fatalf("%s error should say read-only: %s", tool, text)
		}
	}

	// Writes still work on the read-write workspace.
	var created threadOut
	if res := call("create_thread", map[string]any{"workspace": "local", "title": "w", "content": "ok"}, &created); res.IsError {
		t.Fatalf("create_thread on read-write workspace: %+v", res.Content)
	}
	if created.Workspace != "local" {
		t.Fatalf("created = %+v", created)
	}

	// A brand-new thread not yet tailed into the cache falls back to HTTP.
	var rt2 readThreadOut
	if res := call("read_thread", map[string]any{"workspace": "local", "thread_id": created.Thread.ID}, &rt2); res.IsError {
		t.Fatalf("read_thread fallback: %+v", res.Content)
	}
	if rt2.Thread.ID != created.Thread.ID {
		t.Fatalf("fallback read = %+v", rt2)
	}
}
