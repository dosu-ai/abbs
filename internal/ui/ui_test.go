package ui

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
)

type testWorkspace struct {
	server *httptest.Server
	store  *store.Store
	alice  *client.Client
	bob    *client.Client
	thread api.Thread
	dm     api.Thread
}

func newTestWorkspace(t *testing.T, label string, populated bool) *testWorkspace {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st, server.Config{
		WorkspaceName: label,
		// Keep fixture construction outside the reply-loop guard.
		LoopGuardMessages: 100,
	}))
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})

	claim := func(username string) *client.Client {
		c := &client.Client{BaseURL: ts.URL, HTTP: ts.Client()}
		resp, err := c.ClaimUser(t.Context(), api.ClaimUserRequest{Username: username, Kind: "agent"})
		if err != nil {
			t.Fatalf("claim %s: %v", username, err)
		}
		c.Token = resp.Token
		return c
	}
	tw := &testWorkspace{server: ts, store: st, alice: claim("alice"), bob: claim("bob")}
	if !populated {
		return tw
	}

	// Enough activity and tags to exercise the real opaque cursor pagination.
	for i := 0; i < pageLimit; i++ {
		_, err := tw.alice.CreateThread(t.Context(), api.CreateThreadRequest{
			Title:   "Extra thread " + string(rune('A'+i)),
			Content: "fixture",
			Tags:    []string{"extra-" + leftPad(i)},
		})
		if err != nil {
			t.Fatalf("create extra thread %d: %v", i, err)
		}
	}

	tw.thread, err = tw.alice.CreateThread(t.Context(), api.CreateThreadRequest{
		Title:   "Markdown & safety",
		Content: "Initial content",
		Tags:    []string{"architecture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tw.dm, err = tw.alice.CreateThread(t.Context(), api.CreateThreadRequest{
		Title:        "Private coordination",
		Content:      "Secret but visible to this principal",
		Participants: []string{"bob"},
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := tw.alice.ListMessages(t.Context(), tw.thread.ID, "", 1)
	if err != nil || len(first.Items) != 1 {
		t.Fatalf("first message: %+v, %v", first, err)
	}
	unsafeMarkdown := "**bold survives** <img src=x onerror=alert(1)> [bad](javascript:alert(1))"
	if _, err := tw.alice.EditMessage(t.Context(), first.Items[0].ID, unsafeMarkdown); err != nil {
		t.Fatal(err)
	}
	addReaction(t, tw.bob, first.Items[0].ID, "👍")

	// The first message page holds 25 items; a moderator tombstone is the
	// sole item on page two, proving both message paging and deleted_by UI.
	for i := 0; i < pageLimit-1; i++ {
		if _, err := tw.alice.PostMessage(t.Context(), tw.thread.ID, "message "+leftPad(i)); err != nil {
			t.Fatalf("post message %d: %v", i, err)
		}
	}
	last, err := tw.bob.PostMessage(t.Context(), tw.thread.ID, "remove me")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAdmin("alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.alice.DeleteMessage(t.Context(), last.ID); err != nil {
		t.Fatal(err)
	}
	return tw
}

func leftPad(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func addReaction(t *testing.T, c *client.Client, messageID, emoji string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		c.BaseURL+"/v1/messages/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(emoji), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add reaction = %d: %s", resp.StatusCode, body)
	}
}

func writeConfig(t *testing.T, path string, workspaces map[string]*testWorkspace) {
	t.Helper()
	var conf strings.Builder
	for name, ws := range workspaces {
		conf.WriteString("[workspaces.")
		conf.WriteString(name)
		conf.WriteString("]\nurl = \"")
		conf.WriteString(ws.server.URL)
		conf.WriteString("\"\ntoken = \"")
		conf.WriteString(ws.alice.Token)
		conf.WriteString("\"\n\n")
	}
	if err := os.WriteFile(path, []byte(conf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newViewer(t *testing.T, configPath string) *httptest.Server {
	t.Helper()
	h, err := New(Config{ConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func getPage(t *testing.T, base, path string, wantStatus int) (http.Header, string) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s = %d, want %d: %s", path, resp.StatusCode, wantStatus, body)
	}
	return resp.Header, string(body)
}

func requireContains(t *testing.T, body string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(body, value) {
			t.Errorf("page does not contain %q", value)
		}
	}
}

var nextPageRE = regexp.MustCompile(`href="([^"]*page=[^"]*)">Next page</a>`)

func nextPage(t *testing.T, body string) string {
	t.Helper()
	match := nextPageRE.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("page has no next-page link")
	}
	return html.UnescapeString(match[1])
}

func TestRoutesAgainstInProcessServer(t *testing.T) {
	ws := newTestWorkspace(t, "Local development", true)
	configPath := filepath.Join(t.TempDir(), "workspaces.toml")
	writeConfig(t, configPath, map[string]*testWorkspace{"local": ws})
	viewer := newViewer(t, configPath)

	header, body := getPage(t, viewer.URL, "/", http.StatusOK)
	requireContains(t, body, "Workspace of origin", "local", "Local development", "Reachable", "Browse threads")
	if got := header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("CSP = %q, want %q", got, contentSecurityPolicy)
	}
	if strings.Contains(body, ws.alice.Token) {
		t.Fatal("workspace credential leaked into overview")
	}

	header, css := getPage(t, viewer.URL, "/static/app.css", http.StatusOK)
	if !strings.HasPrefix(header.Get("Content-Type"), "text/css") || !strings.Contains(css, ".workspace-grid") {
		t.Fatalf("embedded stylesheet not served: %q", header.Get("Content-Type"))
	}

	_, body = getPage(t, viewer.URL, "/workspaces/local/threads", http.StatusOK)
	requireContains(t, body, "Workspace of origin", "Private coordination", "DM", "Next page")
	threadsPageTwo := nextPage(t, body)
	_, body = getPage(t, viewer.URL, threadsPageTwo, http.StatusOK)
	requireContains(t, body, "First page")

	_, body = getPage(t, viewer.URL, "/workspaces/local/threads?tag=architecture", http.StatusOK)
	requireContains(t, body, "Markdown &amp; safety", "architecture")
	if strings.Contains(body, "Private coordination") {
		t.Fatal("tag filter did not narrow the thread list")
	}

	threadPath := threadURL("local", ws.thread.ID, "")
	_, body = getPage(t, viewer.URL, threadPath, http.StatusOK)
	requireContains(t, body, "Workspace of origin", "bold survives", "<strong>bold survives</strong>", "Edited", "👍", "Next page")
	for _, dangerous := range []string{"onerror", "javascript:", ws.alice.Token} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(dangerous)) {
			t.Fatalf("thread page contains unsafe or secret value %q", dangerous)
		}
	}
	messagePageTwo := nextPage(t, body)
	_, body = getPage(t, viewer.URL, messagePageTwo, http.StatusOK)
	requireContains(t, body, "removed in moderation by @alice", "First page")

	_, body = getPage(t, viewer.URL, "/workspaces/local/tags", http.StatusOK)
	requireContains(t, body, "architecture", "thread", "Next page")
	tagPageTwo := nextPage(t, body)
	_, body = getPage(t, viewer.URL, tagPageTwo, http.StatusOK)
	requireContains(t, body, "First page")

	_, _ = getPage(t, viewer.URL, "/workspaces/local/threads/not-a-thread", http.StatusNotFound)
	_, _ = getPage(t, viewer.URL, "/workspaces/missing/threads", http.StatusNotFound)
	_, _ = getPage(t, viewer.URL, "/not-a-route", http.StatusNotFound)

	req, err := http.NewRequest(http.MethodPost, viewer.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d, want 405", resp.StatusCode)
	}
}

func TestConfigReloadAndPartialOutage(t *testing.T) {
	one := newTestWorkspace(t, "One", false)
	two := newTestWorkspace(t, "Two", false)
	configPath := filepath.Join(t.TempDir(), "workspaces.toml")
	writeConfig(t, configPath, map[string]*testWorkspace{"one": one})
	viewer := newViewer(t, configPath)

	_, body := getPage(t, viewer.URL, "/", http.StatusOK)
	requireContains(t, body, "One")
	if strings.Contains(body, "Two") {
		t.Fatal("unconfigured workspace appeared")
	}

	// The same UI process sees a TOML append on the next request.
	writeConfig(t, configPath, map[string]*testWorkspace{"one": one, "two": two})
	_, body = getPage(t, viewer.URL, "/", http.StatusOK)
	requireContains(t, body, "One", "Two")

	// One dead server becomes an error row; the healthy workspace and its
	// navigation remain usable.
	two.server.Close()
	_, body = getPage(t, viewer.URL, "/", http.StatusOK)
	requireContains(t, body, "One", "two", "Unavailable", "Browse threads")
	_, body = getPage(t, viewer.URL, "/workspaces/one/threads", http.StatusOK)
	requireContains(t, body, "Workspace of origin", "One")

	// A third profile is another append + refresh, still without New/restart.
	writeConfig(t, configPath, map[string]*testWorkspace{"one": one, "two": two, "three": one})
	_, body = getPage(t, viewer.URL, "/", http.StatusOK)
	requireContains(t, body, "one", "two", "three")
}

func TestConfigCanBeFixedAfterStartupAndFallbackWorks(t *testing.T) {
	t.Setenv("ABBS_TOKEN", "")
	ws := newTestWorkspace(t, "Fallback", false)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "workspaces.toml")
	viewer := newViewer(t, configPath)
	_, body := getPage(t, viewer.URL, "/", http.StatusInternalServerError)
	requireContains(t, body, "Configuration unavailable", "ABBS_TOKEN is empty")

	writeConfig(t, configPath, map[string]*testWorkspace{"fixed": ws})
	_, body = getPage(t, viewer.URL, "/", http.StatusOK)
	requireContains(t, body, "fixed", "Fallback")

	fallbackPath := filepath.Join(dir, "still-missing.toml")
	h, err := New(Config{
		ConfigPath: fallbackPath, FallbackURL: ws.server.URL, FallbackToken: ws.alice.Token,
	})
	if err != nil {
		t.Fatal(err)
	}
	fallbackViewer := httptest.NewServer(h)
	defer fallbackViewer.Close()
	_, body = getPage(t, fallbackViewer.URL, "/", http.StatusOK)
	requireContains(t, body, "default", "Fallback")

	t.Setenv("ABBS_URL", ws.server.URL)
	t.Setenv("ABBS_TOKEN", ws.alice.Token)
	envHandler, err := New(Config{ConfigPath: filepath.Join(dir, "env-missing.toml")})
	if err != nil {
		t.Fatal(err)
	}
	envViewer := httptest.NewServer(envHandler)
	defer envViewer.Close()
	_, body = getPage(t, envViewer.URL, "/", http.StatusOK)
	requireContains(t, body, "default", "Fallback")
}

func TestOverviewShowsAdvertisedCapabilities(t *testing.T) {
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/server" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ServerInfo{
			APIVersion:   "v1",
			Workspace:    api.Workspace{Name: "Capable"},
			AuthModes:    []string{"first-claim"},
			Capabilities: []string{"websocket", "future-read-transport"},
			Limits:       api.DefaultLimits(),
		})
	}))
	defer discovery.Close()

	configPath := filepath.Join(t.TempDir(), "workspaces.toml")
	conf := "[workspaces.capable]\nurl = \"" + discovery.URL + "\"\ntoken = \"test-token\"\n"
	if err := os.WriteFile(configPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	viewer := newViewer(t, configPath)
	_, body := getPage(t, viewer.URL, "/", http.StatusOK)
	requireContains(t, body, "Capable", "websocket", "future-read-transport")
}

func TestMarkdownXSSCorpus(t *testing.T) {
	renderer := newMarkdownRenderer()
	corpus := []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`[click](javascript:alert(1))`,
		`[click](vbscript:msgbox(1))`,
		`![image](data:text/html,<script>alert(1)</script>)`,
		`<svg><script>alert(1)</script></svg>`,
		`<math><a href="javascript:alert(1)">x</a></math>`,
		`<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
		`<details open ontoggle=alert(1)>x</details>`,
		`<a href="java&#x73;cript:alert(1)">encoded</a>`,
	}
	for _, payload := range corpus {
		got := strings.ToLower(string(renderer.Render(payload)))
		for _, forbidden := range []string{
			"<script", "javascript:", "vbscript:", "data:text/html", "onerror", "ontoggle", "<svg", "<math", "<iframe", "srcdoc",
		} {
			if strings.Contains(got, forbidden) {
				t.Errorf("rendered XSS corpus entry contains %q\nsource: %s\noutput: %s", forbidden, payload, got)
			}
		}
	}

	got := string(renderer.Render("**strong** and `code`"))
	if !strings.Contains(got, "<strong>strong</strong>") || !strings.Contains(got, "<code>code</code>") {
		t.Fatalf("safe markdown was not rendered: %s", got)
	}
}
