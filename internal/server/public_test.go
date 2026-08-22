package server

import (
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

func TestWorkspaceConfigValidation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tests := []struct {
		name string
		cfg  Config
	}{
		{"name too long", Config{WorkspaceName: strings.Repeat("界", 101)}},
		{"description too long", Config{WorkspaceDescription: strings.Repeat("界", 1001)}},
		{"unknown visibility", Config{WorkspaceVisibility: "internet"}},
		{"unknown auth", Config{AuthMode: "magic"}},
		{"public missing canonical", Config{WorkspaceVisibility: VisibilityPublic}},
		{"private listing", Config{WorkspaceDirectoryListing: true}},
		{"listed missing description", Config{
			WorkspaceVisibility: VisibilityPublic, WorkspaceCanonicalURL: "https://example.com",
			WorkspaceDirectoryListing: true,
		}},
		{"canonical http", Config{WorkspaceCanonicalURL: "http://example.com"}},
		{"canonical credentials", Config{WorkspaceCanonicalURL: "https://user@example.com"}},
		{"canonical path", Config{WorkspaceCanonicalURL: "https://example.com/abbs"}},
		{"canonical query", Config{WorkspaceCanonicalURL: "https://example.com?x=1"}},
		{"canonical fragment", Config{WorkspaceCanonicalURL: "https://example.com#x"}},
		{"canonical uppercase scheme", Config{WorkspaceCanonicalURL: "HTTPS://example.com"}},
		{"canonical malformed", Config{WorkspaceCanonicalURL: "://"}},
		{"invalid trusted proxy", Config{TrustedProxyCIDRs: []string{"127.0.0.1"}}},
		{"invalid write burst", Config{WriteBurst: -1}},
		{"invalid anonymous refill", Config{AnonymousRefillPerSec: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(st, tt.cfg); err == nil {
				t.Fatalf("New(%+v) succeeded", tt.cfg)
			}
		})
	}

	valid := []Config{
		{},
		{WorkspaceCanonicalURL: "https://private.example/"},
		{TrustedProxyCIDRs: []string{"127.0.0.1/32", "::1/128"}},
		{WorkspaceVisibility: VisibilityPublic, WorkspaceCanonicalURL: "https://public.example"},
		{
			WorkspaceName: "公開", WorkspaceDescription: "preserved presentation",
			WorkspaceVisibility: VisibilityPublic, WorkspaceCanonicalURL: "https://public.example/",
			WorkspaceDirectoryListing: true,
		},
	}
	for _, cfg := range valid {
		if _, err := New(st, cfg); err != nil {
			t.Errorf("New(%+v): %v", cfg, err)
		}
	}
}

func TestPublicWorkspaceAnonymousReads(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := MustNew(st, Config{
		WorkspaceName: "公開", WorkspaceDescription: "plain *text*",
		WorkspaceVisibility: VisibilityPublic, WorkspaceCanonicalURL: "https://public.example",
	})
	ts := httptest.NewServer(h)
	t.Cleanup(func() { ts.Close(); st.Close() })

	alice := claim(t, ts.URL, "alice")
	bob := claim(t, ts.URL, "bob")
	var publicThread, dm api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{
		Title: "public", Content: "hello", Tags: []string{"visible"},
	}, http.StatusCreated, &publicThread)
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{
		Title: "secret", Content: "no", Tags: []string{"secret"}, Participants: []string{"bob"},
	}, http.StatusCreated, &dm)

	anon := &client{t: t, base: ts.URL}
	var info api.ServerInfo
	anon.do("GET", "/v1/server", nil, http.StatusOK, &info)
	if info.Workspace.Visibility != VisibilityPublic || info.Workspace.CanonicalURL == nil ||
		*info.Workspace.CanonicalURL != "https://public.example" || info.Workspace.DirectoryListing {
		t.Fatalf("discovery: %+v", info.Workspace)
	}
	var threads api.ThreadPage
	anon.do("GET", "/v1/threads", nil, http.StatusOK, &threads)
	if len(threads.Items) != 1 || threads.Items[0].ID != publicThread.ID {
		t.Fatalf("anonymous threads: %+v", threads.Items)
	}
	anon.do("GET", "/v1/threads/"+publicThread.ID, nil, http.StatusOK, &api.Thread{})
	anon.do("GET", "/v1/threads/"+publicThread.ID+"/messages", nil, http.StatusOK, &api.MessagePage{})
	anon.do("GET", "/v1/threads/"+dm.ID, nil, http.StatusNotFound, nil)
	anon.do("GET", "/v1/threads/"+dm.ID+"/messages", nil, http.StatusNotFound, nil)

	var tags api.TagPage
	anon.do("GET", "/v1/tags", nil, http.StatusOK, &tags)
	if len(tags.Items) != 1 || tags.Items[0].Name != "visible" {
		t.Fatalf("anonymous tags: %+v", tags.Items)
	}

	resp, err := http.Get(ts.URL + "/v1/users/alice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var profile map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		t.Fatal(err)
	}
	if len(profile) != 2 || profile["username"] != "alice" || profile["kind"] != "agent" {
		t.Fatalf("public profile: %+v", profile)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/v1/threads", nil)
	req.Header.Set("Authorization", "Basic nope")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("malformed bearer = %d, want 401", resp.StatusCode)
	}

	_ = bob
}

func TestPrivateWorkspaceConditionalReadsRequireAuth(t *testing.T) {
	ts, _ := newServer(t)
	alice := claim(t, ts.URL, "alice")
	var thread api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "x", Content: "x"}, http.StatusCreated, &thread)
	anon := &client{t: t, base: ts.URL}
	for _, path := range []string{
		"/v1/users/alice", "/v1/threads", "/v1/threads/" + thread.ID,
		"/v1/threads/" + thread.ID + "/messages", "/v1/tags",
	} {
		anon.do("GET", path, nil, http.StatusUnauthorized, nil)
	}
}

func TestAnonymousRateLimitBoundaryAndRefill(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := MustNew(st, Config{AnonymousBurst: 2, AnonymousRefillPerSec: 2})
	ts := httptest.NewServer(h)
	t.Cleanup(func() { ts.Close(); st.Close() })

	get := func() *http.Response {
		resp, err := http.Get(ts.URL + "/v1/server")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if got := get().StatusCode; got != http.StatusOK {
		t.Fatalf("first = %d", got)
	}
	if got := get().StatusCode; got != http.StatusOK {
		t.Fatalf("boundary = %d", got)
	}
	limited := get()
	if limited.StatusCode != http.StatusTooManyRequests || limited.Header.Get("Retry-After") == "" {
		t.Fatalf("limited = %d retry=%q", limited.StatusCode, limited.Header.Get("Retry-After"))
	}
	time.Sleep(600 * time.Millisecond)
	if got := get().StatusCode; got != http.StatusOK {
		t.Fatalf("after refill = %d", got)
	}
}
