package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/cache"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
	"github.com/dosu-ai/abbs/internal/workspace"
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
		w := &Workspace{Name: name, Label: name, URL: url, Client: cl, Cache: ca, ReadOnly: readOnly}
		w.markReady()
		return w
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

func TestUnavailableWorkspaceRemainsVisibleAndDoesNotBecomeUnambiguous(t *testing.T) {
	healthy := &Workspace{Name: "healthy", Label: "Healthy", URL: "http://healthy", Client: &client.Client{BaseURL: "http://healthy"}}
	bad := &Workspace{Name: "bad", URL: "http://bad"}
	bad.setUnavailable(errors.New("connection refused"))
	a := &adapter{
		workspaces: map[string]*Workspace{"bad": bad, "healthy": healthy},
		names:      []string{"bad", "healthy"},
	}

	if got, err := a.resolve(""); err == nil || got != nil || !strings.Contains(err.Error(), "several workspaces") {
		t.Fatalf("resolve omitted workspace = %v, %v", got, err)
	}
	if _, err := a.resolve("bad"); err == nil || !strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "unknown workspace") {
		t.Fatalf("resolve unavailable workspace error = %v", err)
	}
	single := &adapter{workspaces: map[string]*Workspace{"bad": bad}, names: []string{"bad"}}
	if got, err := single.resolve(""); err == nil || got != nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("resolve sole configured unavailable workspace = %v, %v", got, err)
	}

	_, out, err := a.listWorkspaces(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Workspaces) != 2 || out.Workspaces[0].Available || out.Workspaces[0].Error != "connection refused" || !out.Workspaces[1].Available {
		t.Fatalf("list_workspaces = %+v", out.Workspaces)
	}
}

func TestReadsUseHTTPUntilFreshCacheIsReady(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.MustNew(st, server.Config{WorkspaceName: "truth"}))
	t.Cleanup(func() { ts.Close(); st.Close() })

	anon := &client.Client{BaseURL: ts.URL}
	claim, err := anon.ClaimUser(ctx, api.ClaimUserRequest{Username: "me", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	cl := &client.Client{BaseURL: ts.URL, Token: claim.Token}
	created, err := cl.CreateThread(ctx, api.CreateThreadRequest{Title: "server truth", Content: "not cached yet"})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := cache.Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	if cached, err := ca.ListThreads(cache.ListThreadsOptions{}); err != nil || len(cached.Items) != 0 {
		t.Fatalf("fresh cache = %+v, %v", cached, err)
	}

	w := &Workspace{Name: "truth", Label: "truth", URL: ts.URL, Client: cl, Cache: ca}
	a := &adapter{workspaces: map[string]*Workspace{"truth": w}, names: []string{"truth"}}
	_, threads, err := a.listThreads(ctx, nil, listThreadsIn{})
	if err != nil || len(threads.Items) != 1 || threads.Items[0].ID != created.ID {
		t.Fatalf("list_threads before cache ready = %+v, %v", threads, err)
	}
	_, thread, err := a.readThread(ctx, nil, readThreadIn{ThreadID: created.ID})
	if err != nil || len(thread.Messages) != 1 || thread.Messages[0].Content != "not cached yet" {
		t.Fatalf("read_thread before cache ready = %+v, %v", thread, err)
	}
}

func TestCacheOpenFailureFallsBackToHTTP(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.MustNew(st, server.Config{WorkspaceName: "direct"}))
	t.Cleanup(func() { ts.Close(); st.Close() })
	claim, err := (&client.Client{BaseURL: ts.URL}).ClaimUser(ctx, api.ClaimUserRequest{Username: "me", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	cl := &client.Client{BaseURL: ts.URL, Token: claim.Token}
	if _, err := cl.CreateThread(ctx, api.CreateThreadRequest{Title: "direct read", Content: "ok"}); err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	w := &Workspace{Name: "direct", URL: ts.URL}
	runtime := &workspaceRuntime{
		w: w, profile: workspace.Profile{URL: ts.URL, Token: claim.Token},
		logf:      func(_ string, format string, args ...any) { logs.WriteString(format) },
		cachePath: func(_, _, _ string) (string, error) { return "broken.db", nil },
		openCache: func(string) (*cache.Cache, error) { return nil, errors.New("permission denied") },
	}
	if err := runtime.connect(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Cache != nil || w.unavailableError() != nil || !strings.Contains(logs.String(), "serving reads directly over HTTP") {
		t.Fatalf("degraded cache state: cache=%v unavailable=%v logs=%q", w.Cache, w.unavailableError(), logs.String())
	}
	a := &adapter{workspaces: map[string]*Workspace{"direct": w}, names: []string{"direct"}}
	_, out, err := a.listThreads(ctx, nil, listThreadsIn{})
	if err != nil || len(out.Items) != 1 || out.Items[0].Title != "direct read" {
		t.Fatalf("HTTP fallback = %+v, %v", out, err)
	}
}

func TestBootstrapRunsAfterWorkspaceStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	base := server.MustNew(st, server.Config{WorkspaceName: "async"})
	bootstrapStarted := make(chan struct{})
	releaseBootstrap := make(chan struct{})
	var block atomic.Bool
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() && r.Method == http.MethodGet && r.URL.Path == "/v1/threads" {
			once.Do(func() { close(bootstrapStarted) })
			select {
			case <-releaseBootstrap:
			case <-r.Context().Done():
				return
			}
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { ts.Close(); st.Close() })
	claim, err := (&client.Client{BaseURL: ts.URL}).ClaimUser(ctx, api.ClaimUserRequest{Username: "me", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)

	w := &Workspace{Name: "async", URL: ts.URL}
	runtime := &workspaceRuntime{
		w: w, profile: workspace.Profile{URL: ts.URL, Token: claim.Token},
		cachePath: func(_, _, _ string) (string, error) { return filepath.Join(t.TempDir(), "cache.db"), nil },
	}
	if err := runtime.connect(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bootstrapStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background bootstrap did not start")
	}
	if w.cacheForRead() != nil {
		t.Fatal("cache became readable before bootstrap committed")
	}
	close(releaseBootstrap)
	deadline := time.Now().Add(2 * time.Second)
	for w.cacheForRead() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.cacheForRead() == nil {
		t.Fatal("cache did not become ready after bootstrap committed")
	}
}

func TestUnavailableWorkspaceRecoversWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	base := server.MustNew(st, server.Config{WorkspaceName: "recovered"})
	var up atomic.Bool
	up.Store(true)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "temporarily down", http.StatusServiceUnavailable)
			return
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { ts.Close(); st.Close() })
	claim, err := (&client.Client{BaseURL: ts.URL}).ClaimUser(ctx, api.ClaimUserRequest{Username: "me", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	up.Store(false)

	healthyStore, err := store.Open(filepath.Join(t.TempDir(), "healthy.db"))
	if err != nil {
		t.Fatal(err)
	}
	healthyTS := httptest.NewServer(server.MustNew(healthyStore, server.Config{WorkspaceName: "healthy"}))
	t.Cleanup(func() { healthyTS.Close(); healthyStore.Close() })
	healthyClaim, err := (&client.Client{BaseURL: healthyTS.URL}).ClaimUser(ctx, api.ClaimUserRequest{Username: "me", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}

	profiles := map[string]workspace.Profile{
		"flaky":   {URL: ts.URL, Token: claim.Token},
		"healthy": {URL: healthyTS.URL, Token: healthyClaim.Token},
	}
	wss, err := initializeWorkspaces(ctx, profiles, []string{"flaky", "healthy"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &adapter{workspaces: map[string]*Workspace{"flaky": wss[0], "healthy": wss[1]}, names: []string{"flaky", "healthy"}}
	if got, err := a.resolve(""); err == nil || got != nil || !strings.Contains(err.Error(), "several workspaces") {
		t.Fatalf("resolve while flaky is down = %v, %v", got, err)
	}
	if _, err := a.resolve("flaky"); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("flaky error = %v", err)
	}

	up.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for wss[0].unavailableError() != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := wss[0].unavailableError(); err != nil {
		t.Fatalf("workspace did not recover: %v", err)
	}
	if _, err := a.resolve("flaky"); err != nil {
		t.Fatalf("resolve recovered workspace: %v", err)
	}
	if _, err := a.resolve(""); err == nil || !strings.Contains(err.Error(), "several workspaces") {
		t.Fatalf("empty workspace with both recovered = %v", err)
	}
}

func TestInvalidCredentialStaysUnavailableAndTokenFileCorrectionRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.MustNew(st, server.Config{WorkspaceName: "authenticated"}))
	t.Cleanup(func() { ts.Close(); st.Close() })
	claim, err := (&client.Client{BaseURL: ts.URL}).ClaimUser(ctx, api.ClaimUserRequest{Username: "me", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("invalid-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	profiles := map[string]workspace.Profile{
		"invalid": {URL: ts.URL, TokenFile: tokenFile},
		"healthy": {URL: ts.URL, Token: claim.Token},
	}
	wss, err := initializeWorkspaces(ctx, profiles, []string{"invalid", "healthy"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wss[0].unavailableError(); err == nil || !strings.Contains(err.Error(), "cannot authenticate") || !strings.Contains(err.Error(), "unknown token") {
		t.Fatalf("invalid credential availability = %v", err)
	}
	wss[0].mu.RLock()
	invalidClient, invalidLabel := wss[0].Client, wss[0].Label
	wss[0].mu.RUnlock()
	if invalidClient != nil || invalidLabel != "" {
		t.Fatalf("invalid credential published workspace: client=%v label=%q", invalidClient, invalidLabel)
	}

	if err := os.WriteFile(tokenFile, []byte(claim.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for wss[0].unavailableError() != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := wss[0].unavailableError(); err != nil {
		t.Fatalf("workspace did not recover after token-file correction: %v", err)
	}
	if _, err := wss[0].clientForRequest(); err != nil || wss[0].info().Label != "authenticated" {
		t.Fatalf("corrected credential was not published: info=%+v err=%v", wss[0].info(), err)
	}
}

func TestConnectAttemptHasOverallDeadline(t *testing.T) {
	requestStarted := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requestStarted) })
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	runtime := &workspaceRuntime{
		w:              &Workspace{Name: "hanging", URL: ts.URL},
		profile:        workspace.Profile{URL: ts.URL, Token: "token"},
		noCache:        true,
		connectTimeout: 250 * time.Millisecond,
	}
	started := time.Now()
	err := runtime.connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("bounded connect error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded connect took %s", elapsed)
	}
	select {
	case <-requestStarted:
	default:
		t.Fatal("hanging discovery request never started")
	}
}

func TestInitializeWorkspacesConnectsProfilesConcurrently(t *testing.T) {
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hanging.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "healthy.db"))
	if err != nil {
		t.Fatal(err)
	}
	healthy := httptest.NewServer(server.MustNew(st, server.Config{WorkspaceName: "healthy"}))
	t.Cleanup(func() { healthy.Close(); st.Close() })
	claim, err := (&client.Client{BaseURL: healthy.URL}).ClaimUser(context.Background(), api.ClaimUserRequest{Username: "me", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	profiles := map[string]workspace.Profile{
		"hanging": {URL: hanging.URL, Token: "token"},
		"healthy": {URL: healthy.URL, Token: claim.Token},
	}
	started := time.Now()
	wss, err := initializeWorkspaces(ctx, profiles, []string{"hanging", "healthy"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("concurrent initialization took %s", elapsed)
	}
	if len(wss) != 2 || wss[0].unavailableError() == nil || wss[1].unavailableError() != nil || wss[1].Client == nil {
		t.Fatalf("initialized workspaces = %+v", wss)
	}
}

func TestNoAvailableWorkspaceListsEveryFailure(t *testing.T) {
	one := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	two := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	oneURL, twoURL := one.URL, two.URL
	one.Close()
	two.Close()
	profiles := map[string]workspace.Profile{
		"one": {URL: oneURL, Token: "token"},
		"two": {URL: twoURL, Token: "token"},
	}
	_, err := initializeWorkspaces(context.Background(), profiles, []string{"one", "two"}, true, nil)
	if err == nil || !strings.Contains(err.Error(), "one: cannot reach server") || !strings.Contains(err.Error(), "two: cannot reach server") {
		t.Fatalf("all-unavailable error = %v", err)
	}
}
