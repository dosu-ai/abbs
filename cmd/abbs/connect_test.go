package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
	"github.com/dosu-ai/abbs/internal/workspace"
)

type connectBoard struct {
	server *httptest.Server
	store  *store.Store
}

func newConnectBoard(t *testing.T, name string) *connectBoard {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := server.MustNew(st, server.Config{
		WorkspaceName:         name,
		WorkspaceVisibility:   server.VisibilityPublic,
		WorkspaceCanonicalURL: "https://board.example",
	})
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return &connectBoard{server: ts, store: st}
}

func TestConnectFreshJSONAndIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABBS_CONFIG", "")
	board := newConnectBoard(t, "Agent Commons")
	configPath := filepath.Join(home, ".config", "abbs", "workspaces.toml")
	args := []string{board.server.URL + "/", "-username", "alice", "-kind", "agent", "-display-name", "Alice Agent", "-read-only", "-json"}

	var stdout, stderr bytes.Buffer
	if code := runConnect(args, &stdout, &stderr); code != connectOK {
		t.Fatalf("first connect exit = %d, stderr = %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("first connect stderr = %q", stderr.String())
	}
	var first connectResult
	if err := json.Unmarshal(stdout.Bytes(), &first); err != nil {
		t.Fatalf("JSON output = %q: %v", stdout.String(), err)
	}
	if first.Profile != "agent-commons" || first.URL != board.server.URL || first.Workspace != "Agent Commons" || first.Username != "alice" || first.AlreadyConnected {
		t.Fatalf("first result = %#v", first)
	}

	profiles, names, err := workspace.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["agent-commons"]
	if len(names) != 1 || p.URL != board.server.URL || p.Username != "alice" || !p.ReadOnly {
		t.Fatalf("profile = %#v, names = %v", p, names)
	}
	token, err := p.ResolveToken()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || strings.Contains(stdout.String(), token) {
		t.Fatalf("token leaked or missing; stdout = %q", stdout.String())
	}
	assertConnectPerm(t, filepath.Dir(configPath), 0o700)
	assertConnectPerm(t, configPath, 0o600)
	assertConnectPerm(t, p.TokenFile, 0o600)

	configBefore, _ := os.ReadFile(configPath)
	tokenBefore, _ := os.ReadFile(p.TokenFile)
	stdout.Reset()
	stderr.Reset()
	if code := runConnect(args, &stdout, &stderr); code != connectOK {
		t.Fatalf("second connect exit = %d, stderr = %s", code, stderr.String())
	}
	var second connectResult
	if err := json.Unmarshal(stdout.Bytes(), &second); err != nil {
		t.Fatalf("second JSON output = %q: %v", stdout.String(), err)
	}
	if !second.AlreadyConnected || second.Username != "alice" {
		t.Fatalf("second result = %#v", second)
	}
	configAfter, _ := os.ReadFile(configPath)
	tokenAfter, _ := os.ReadFile(p.TokenFile)
	if !bytes.Equal(configBefore, configAfter) || !bytes.Equal(tokenBefore, tokenAfter) {
		t.Fatal("idempotent reconnect changed config or token")
	}
}

func TestConnectHumanOutputAndPrintToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABBS_CONFIG", "")
	board := newConnectBoard(t, "Board")
	configPath := filepath.Join(t.TempDir(), "profiles.toml")
	var stdout, stderr bytes.Buffer
	code := runConnect([]string{board.server.URL, "-username", "bob", "-config", configPath, "-print-token"}, &stdout, &stderr)
	if code != connectOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	profiles, _, err := workspace.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := profiles["board"].ResolveToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Token: "+token) || !strings.Contains(stdout.String(), "claude mcp add abbs -- abbs mcp -config") {
		t.Fatalf("human output = %q", stdout.String())
	}
}

func TestConnectUsernameTakenSuggestsFreeAlternatesAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABBS_CONFIG", "")
	board := newConnectBoard(t, "Public Board")
	anon := &client.Client{BaseURL: board.server.URL}
	for _, name := range []string{"alice", "alice-2"} {
		if _, err := anon.ClaimUser(context.Background(), api.ClaimUserRequest{Username: name, Kind: "agent"}); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(home, ".config", "abbs", "workspaces.toml")
	var stdout, stderr bytes.Buffer
	code := runConnect([]string{board.server.URL, "-username", "alice"}, &stdout, &stderr)
	if code != connectUsernameTaken {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "alice-3, alice-4") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config should not exist: %v", err)
	}
	tokenPath, err := workspace.TokenPath("public-board")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("token should not exist: %v", err)
	}
}

func TestConnectProfileCollisionWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABBS_CONFIG", "")
	board := newConnectBoard(t, "Board")
	configPath := filepath.Join(t.TempDir(), "profiles.toml")
	original := []byte("# keep\n[workspaces.board]\nurl = \"https://other.example\"\ntoken = \"existing\"\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runConnect([]string{board.server.URL, "-username", "alice", "-config", configPath}, &stdout, &stderr)
	if code != connectUsageError || !strings.Contains(stderr.String(), "choose another name with -as") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	got, _ := os.ReadFile(configPath)
	if !bytes.Equal(got, original) {
		t.Fatal("collision changed config")
	}
	tokenPath, _ := workspace.TokenPath("board")
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("collision wrote token: %v", err)
	}
}

func TestConnectSecondBoardPreservesFirstProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABBS_CONFIG", "")
	first := newConnectBoard(t, "First")
	second := newConnectBoard(t, "Second")
	configPath := filepath.Join(home, ".config", "abbs", "workspaces.toml")

	var stdout, stderr bytes.Buffer
	if code := runConnect([]string{first.server.URL, "-username", "first-bot"}, &stdout, &stderr); code != connectOK {
		t.Fatalf("first exit = %d, stderr = %s", code, stderr.String())
	}
	firstBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runConnect([]string{second.server.URL, "-username", "second-bot"}, &stdout, &stderr); code != connectOK {
		t.Fatalf("second exit = %d, stderr = %s", code, stderr.String())
	}
	allBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(allBytes, firstBytes) {
		t.Fatalf("first profile changed while adding second:\nbefore:\n%s\nafter:\n%s", firstBytes, allBytes)
	}
	profiles, names, err := workspace.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || profiles["first"].Username != "first-bot" || profiles["second"].Username != "second-bot" {
		t.Fatalf("profiles = %#v, names = %v", profiles, names)
	}
}

func TestConnectUnreachableAndNonConformingExitTwo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABBS_CONFIG", "")
	nonConforming := httptest.NewServer(httpHandler(`{"api_version":"not-v1"}`))
	defer nonConforming.Close()
	for _, tt := range []struct {
		name string
		url  string
	}{
		{name: "unreachable", url: "http://127.0.0.1:1"},
		{name: "non-conforming", url: nonConforming.URL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runConnect([]string{tt.url, "-username", "alice"}, &stdout, &stderr)
			if code != connectServerError || stdout.Len() != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestConnectDiscoverySurfacesProblemDetail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABBS_CONFIG", "")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"type":"https://abbs.dev/problems/unavailable","title":"Board unavailable","status":503,"detail":"maintenance window"}`))
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runConnect([]string{ts.URL, "-username", "alice"}, &stdout, &stderr)
	if code != connectServerError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "maintenance window") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestNormalizeConnectURLAndSlug(t *testing.T) {
	for _, raw := range []string{"http://localhost:8080", "http://example.com", "ftp://127.0.0.1/x", "https://user@example.com"} {
		if _, err := normalizeConnectURL(raw); err == nil {
			t.Fatalf("normalizeConnectURL(%q) succeeded", raw)
		}
	}
	if got, err := normalizeConnectURL(" http://127.0.0.1:8080/// "); err != nil || got != "http://127.0.0.1:8080" {
		t.Fatalf("normalized = %q, %v", got, err)
	}
	if got, err := normalizeConnectURL("HTTPS://example.com/"); err != nil || got != "https://example.com" {
		t.Fatalf("uppercase scheme normalized = %q, %v", got, err)
	}
	if got := slugProfile("  Café & Agent BOARD!! "); got != "caf-agent-board" {
		t.Fatalf("slug = %q", got)
	}
}

type staticHandler string

func httpHandler(body string) staticHandler { return staticHandler(body) }

func (h staticHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(h))
}

func assertConnectPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s permissions = %#o, want %#o", path, got, want)
	}
}
