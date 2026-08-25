package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	abbsserver "github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
)

func TestAPIOperationRegistryMatchesOpenAPI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "abbs.openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s+operationId:\s+([A-Za-z0-9]+)\s*$`)
	var specIDs []string
	for _, match := range re.FindAllSubmatch(b, -1) {
		specIDs = append(specIDs, string(match[1]))
	}
	registryIDs := make([]string, 0, len(apiOperations))
	commands := map[string]string{}
	for _, op := range apiOperations {
		registryIDs = append(registryIDs, op.OperationID)
		command := strings.Join(op.CommandPath, " ")
		if previous := commands[command]; previous != "" {
			t.Fatalf("duplicate command %q for %s and %s", command, previous, op.OperationID)
		}
		commands[command] = op.OperationID
		if op.Run == nil {
			t.Fatalf("%s has no runner", op.OperationID)
		}
	}
	sort.Strings(specIDs)
	sort.Strings(registryIDs)
	if !reflect.DeepEqual(registryIDs, specIDs) {
		t.Fatalf("registry IDs do not match spec\nregistry: %v\nspec: %v", registryIDs, specIDs)
	}
	if len(registryIDs) != 31 {
		t.Fatalf("got %d operations, want 31", len(registryIDs))
	}
}

type commandShape struct {
	name          string
	args          []string
	method        string
	escapedPath   string
	query         url.Values
	body          string
	authorization string
	mutation      bool
	noContent     bool
}

func TestAPICommandRequestShapes(t *testing.T) {
	t.Setenv("ABBS_TOKEN", "abbs-secret")
	t.Setenv("ABBS_IDP_TOKEN", "idp-secret")
	t.Setenv("TEST_REFRESH_TOKEN", "refresh-secret")
	thread := "thread id"
	message := "message id"
	tests := []commandShape{
		{"getServer", []string{"server", "get"}, http.MethodGet, "/v1/server", nil, "", "", false, false},
		{"claimUser", []string{"user", "claim", "--username", "alice", "--kind", "human", "--display-name", "Alice", "--idempotency-key", "fixed"}, http.MethodPost, "/v1/users", nil, `{"username":"alice","kind":"human","display_name":"Alice"}`, "Bearer abbs-secret", true, false},
		{"listUsers", []string{"user", "list", "--page", "p1", "--limit", "2"}, http.MethodGet, "/v1/users", url.Values{"page": {"p1"}, "limit": {"2"}}, "", "Bearer abbs-secret", false, false},
		{"getUser", []string{"user", "get", "alice"}, http.MethodGet, "/v1/users/alice", nil, "", "Bearer abbs-secret", false, false},
		{"deactivateUser", []string{"user", "deactivate", "alice", "--yes", "--idempotency-key", "fixed"}, http.MethodPost, "/v1/users/alice/deactivate", nil, "", "Bearer abbs-secret", true, false},
		{"createThread", []string{"thread", "create", "--title", "Decision", "--content", "hello", "--tag", "api", "--tag", "design", "--participant", "bob", "--idempotency-key", "fixed"}, http.MethodPost, "/v1/threads", nil, `{"title":"Decision","content":"hello","tags":["api","design"],"participants":["bob"]}`, "Bearer abbs-secret", true, false},
		{"listThreads", []string{"thread", "list", "--since", "4", "--tag", "api", "--tag", "design", "--page", "p2", "--limit", "3"}, http.MethodGet, "/v1/threads", url.Values{"since": {"4"}, "tag": {"api", "design"}, "page": {"p2"}, "limit": {"3"}}, "", "Bearer abbs-secret", false, false},
		{"getThread", []string{"thread", "get", thread}, http.MethodGet, "/v1/threads/thread%20id", nil, "", "Bearer abbs-secret", false, false},
		{"updateThreadTags", []string{"thread", "set-tags", thread, "--tag", "api", "--idempotency-key", "fixed"}, http.MethodPatch, "/v1/threads/thread%20id", nil, `{"tags":["api"]}`, "Bearer abbs-secret", true, false},
		{"listMessages", []string{"thread", "messages", thread, "--page", "p3", "--limit", "4"}, http.MethodGet, "/v1/threads/thread%20id/messages", url.Values{"page": {"p3"}, "limit": {"4"}}, "", "Bearer abbs-secret", false, false},
		{"postMessage", []string{"thread", "reply", thread, "--content", "reply", "--idempotency-key", "fixed"}, http.MethodPost, "/v1/threads/thread%20id/messages", nil, `{"content":"reply"}`, "Bearer abbs-secret", true, false},
		{"getReadCursor", []string{"thread", "read-cursor", thread}, http.MethodGet, "/v1/threads/thread%20id/read-cursor", nil, "", "Bearer abbs-secret", false, false},
		{"setReadCursor", []string{"thread", "mark-read", thread, "--seq", "9", "--idempotency-key", "fixed"}, http.MethodPut, "/v1/threads/thread%20id/read-cursor", nil, `{"seq":"9"}`, "Bearer abbs-secret", true, true},
		{"getMessage", []string{"message", "get", message}, http.MethodGet, "/v1/messages/message%20id", nil, "", "Bearer abbs-secret", false, false},
		{"editMessage", []string{"message", "edit", message, "--content", "edited", "--idempotency-key", "fixed"}, http.MethodPatch, "/v1/messages/message%20id", nil, `{"content":"edited"}`, "Bearer abbs-secret", true, false},
		{"deleteMessage", []string{"message", "delete", message, "--yes", "--idempotency-key", "fixed"}, http.MethodDelete, "/v1/messages/message%20id", nil, "", "Bearer abbs-secret", true, false},
		{"listReactions", []string{"reaction", "list", message, "--page", "p4", "--limit", "5"}, http.MethodGet, "/v1/messages/message%20id/reactions", url.Values{"page": {"p4"}, "limit": {"5"}}, "", "Bearer abbs-secret", false, false},
		{"addReaction", []string{"reaction", "add", message, "👍🏽", "--idempotency-key", "fixed"}, http.MethodPut, "/v1/messages/message%20id/reactions/%F0%9F%91%8D%F0%9F%8F%BD", nil, "", "Bearer abbs-secret", true, true},
		{"removeReaction", []string{"reaction", "remove", message, "👍🏽", "--idempotency-key", "fixed"}, http.MethodDelete, "/v1/messages/message%20id/reactions/%F0%9F%91%8D%F0%9F%8F%BD", nil, "", "Bearer abbs-secret", true, true},
		{"pollEvents", []string{"event", "poll", "--cursor", "8", "--timeout", "4", "--limit", "6", "--mentions", "--dms", "--subscribed-tags", "--tag", "api"}, http.MethodGet, "/v1/events", url.Values{"cursor": {"8"}, "timeout": {"4"}, "limit": {"6"}, "mentions": {"true"}, "dms": {"true"}, "subscribed_tags": {"true"}, "tag": {"api"}}, "", "Bearer abbs-secret", false, false},
		{"getInbox", []string{"inbox", "list", "--page", "p5", "--limit", "7"}, http.MethodGet, "/v1/inbox", url.Values{"page": {"p5"}, "limit": {"7"}}, "", "Bearer abbs-secret", false, false},
		{"listTags", []string{"tag", "list", "--page", "p6", "--limit", "8"}, http.MethodGet, "/v1/tags", url.Values{"page": {"p6"}, "limit": {"8"}}, "", "Bearer abbs-secret", false, false},
		{"listTagSubscriptions", []string{"tag", "subscription", "list", "--page", "p7", "--limit", "9"}, http.MethodGet, "/v1/tag-subscriptions", url.Values{"page": {"p7"}, "limit": {"9"}}, "", "Bearer abbs-secret", false, false},
		{"subscribeTag", []string{"tag", "subscription", "add", "go/lang", "--idempotency-key", "fixed"}, http.MethodPut, "/v1/tag-subscriptions/go%2Flang", nil, "", "Bearer abbs-secret", true, true},
		{"unsubscribeTag", []string{"tag", "subscription", "remove", "go/lang", "--idempotency-key", "fixed"}, http.MethodDelete, "/v1/tag-subscriptions/go%2Flang", nil, "", "Bearer abbs-secret", true, true},
		{"registerAgent", []string{"agent", "register", "--username", "helper", "--display-name", "Helper", "--idempotency-key", "fixed"}, http.MethodPost, "/v1/agents", nil, `{"username":"helper","display_name":"Helper"}`, "Bearer idp-secret", true, false},
		{"listAgents", []string{"agent", "list", "--page", "p8", "--limit", "10"}, http.MethodGet, "/v1/agents", url.Values{"page": {"p8"}, "limit": {"10"}}, "", "Bearer abbs-secret", false, false},
		{"getAgent", []string{"agent", "get", "helper"}, http.MethodGet, "/v1/agents/helper", nil, "", "Bearer abbs-secret", false, false},
		{"revokeAgentTokens", []string{"agent", "revoke-tokens", "helper", "--yes", "--idempotency-key", "fixed"}, http.MethodDelete, "/v1/agents/helper/tokens", nil, "", "Bearer abbs-secret", true, true},
		{"refreshToken", []string{"token", "refresh", "--refresh-token-env", "TEST_REFRESH_TOKEN", "--idempotency-key", "fixed"}, http.MethodPost, "/v1/tokens/refresh", nil, `{"refresh_token":"refresh-secret"}`, "", true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotMethod, gotPath, gotAuthorization, gotIdempotency string
			var gotQuery url.Values
			var gotBody []byte
			var targetCalls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.name != "getServer" && r.URL.Path == "/v1/server" {
					_, _ = io.WriteString(w, `{"api_version":"v1","workspace":{"name":"test","visibility":"private","directory_listing":false},"auth_modes":["first-claim"],"limits":{}}`)
					return
				}
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				targetCalls++
				gotMethod, gotPath = r.Method, r.URL.EscapedPath()
				gotQuery = r.URL.Query()
				gotBody = body
				gotAuthorization = r.Header.Get("Authorization")
				gotIdempotency = r.Header.Get("Idempotency-Key")
				mu.Unlock()
				if tc.noContent {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				_, _ = io.WriteString(w, ` { "known" : true, "future" : { "value" : 42 } } `)
			}))
			defer srv.Close()

			args := append([]string{"--url", srv.URL}, tc.args...)
			var stdout, stderr bytes.Buffer
			code := runAPIContext(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
			if code != apiOK {
				t.Fatalf("exit %d, stderr: %s", code, stderr.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if targetCalls != 1 {
				t.Fatalf("target calls = %d, want 1", targetCalls)
			}
			if gotMethod != tc.method || gotPath != tc.escapedPath {
				t.Fatalf("got %s %s, want %s %s", gotMethod, gotPath, tc.method, tc.escapedPath)
			}
			if !reflect.DeepEqual(gotQuery, tc.query) && !(len(gotQuery) == 0 && len(tc.query) == 0) {
				t.Fatalf("query = %#v, want %#v", gotQuery, tc.query)
			}
			if tc.body == "" {
				if len(gotBody) != 0 {
					t.Fatalf("unexpected body %s", gotBody)
				}
			} else if !jsonEqual(gotBody, []byte(tc.body)) {
				t.Fatalf("body = %s, want %s", gotBody, tc.body)
			}
			if gotAuthorization != tc.authorization {
				t.Fatalf("Authorization = %q, want %q", gotAuthorization, tc.authorization)
			}
			if tc.mutation && gotIdempotency != "fixed" {
				t.Fatalf("Idempotency-Key = %q, want fixed", gotIdempotency)
			}
			if !tc.mutation && gotIdempotency != "" {
				t.Fatalf("unexpected Idempotency-Key %q", gotIdempotency)
			}
			if tc.noContent {
				if stdout.Len() != 0 {
					t.Fatalf("204 stdout = %q", stdout.String())
				}
			} else if stdout.String() != `{"known":true,"future":{"value":42}}`+"\n" {
				t.Fatalf("stdout did not preserve unknown response fields: %q", stdout.String())
			}
		})
	}
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}

func TestAPIAnonymousReadSuppressesProfileCredential(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"future":true}`)
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	code := runAPIContext(context.Background(), []string{"--url", srv.URL, "--anonymous", "tag", "list"}, strings.NewReader(""), &stdout, &stderr)
	if code != apiOK || auth != "" {
		t.Fatalf("exit=%d auth=%q stderr=%s", code, auth, stderr.String())
	}
}

func TestAPIJSONProblemAndStableHTTPExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		if r.URL.Path == "/v1/server" {
			_, _ = io.WriteString(w, `{"api_version":"v1","workspace":{"name":"test","visibility":"private","directory_listing":false},"auth_modes":["first-claim"],"limits":{}}`)
			return
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, ` {"type":"urn:test","title":"Nope","status":418,"future":"kept"} `)
	}))
	defer srv.Close()
	t.Setenv("ABBS_TOKEN", "secret")
	var stdout, stderr bytes.Buffer
	code := runAPIContext(context.Background(), []string{"--url", srv.URL, "--json-errors", "user", "list"}, strings.NewReader(""), &stdout, &stderr)
	if code != apiHTTPExit {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 || stderr.String() != `{"type":"urn:test","title":"Nope","status":418,"future":"kept"}`+"\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestAPISecretRedactionIncludesRawProblemMode(t *testing.T) {
	t.Setenv("TEST_REFRESH_TOKEN", "refresh-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"urn:auth","title":"Bad token","status":401,"detail":"rejected refresh-secret","future":"kept"}`)
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"--url", srv.URL, "--json-errors", "token", "refresh", "--refresh-token-env", "TEST_REFRESH_TOKEN"}
	code := runAPIContext(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	if code != apiHTTPExit || strings.Contains(stderr.String(), "refresh-secret") || !strings.Contains(stderr.String(), "[REDACTED]") || !strings.Contains(stderr.String(), `"future":"kept"`) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestAPIReadOnlyRejectsMutationBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()
	config := filepath.Join(t.TempDir(), "workspaces.toml")
	contents := fmt.Sprintf("[workspaces.board]\nurl = %q\ntoken = %q\nread_only = true\n", srv.URL, "secret")
	if err := os.WriteFile(config, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runAPIContext(context.Background(), []string{"--config", config, "thread", "set-tags", "id"}, strings.NewReader(""), &stdout, &stderr)
	if code != apiUsageExit || calls.Load() != 0 || !strings.Contains(stderr.String(), "read-only") {
		t.Fatalf("exit=%d calls=%d stderr=%s", code, calls.Load(), stderr.String())
	}
}

func TestAPIContentFromStdinAndClearTags(t *testing.T) {
	t.Setenv("ABBS_TOKEN", "secret")
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/server" {
			_, _ = io.WriteString(w, `{"api_version":"v1","workspace":{"name":"test","visibility":"private","directory_listing":false},"auth_modes":["first-claim"],"limits":{}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		args  []string
		stdin string
	}{
		{[]string{"--url", srv.URL, "thread", "reply", "id", "--content-file", "-"}, "from stdin\n"},
		{[]string{"--url", srv.URL, "thread", "set-tags", "id"}, ""},
	} {
		var stdout, stderr bytes.Buffer
		if code := runAPIContext(context.Background(), tc.args, strings.NewReader(tc.stdin), &stdout, &stderr); code != apiOK {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
	}
	if !jsonEqual(bodies[0], []byte(`{"content":"from stdin\n"}`)) || !jsonEqual(bodies[1], []byte(`{"tags":[]}`)) {
		t.Fatalf("bodies = %q", bodies)
	}
}

func TestAPIEventStreamJSONLUnknownFields(t *testing.T) {
	t.Setenv("ABBS_TOKEN", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"api_version":"v1","workspace":{"name":"test","visibility":"private","directory_listing":false},"auth_modes":["first-claim"],"capabilities":["websocket"],"limits":{}}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		wantQuery := url.Values{"cursor": {"8"}, "mentions": {"true"}, "tag": {"api", "design"}}
		if got := r.URL.Query(); !reflect.DeepEqual(got, wantQuery) {
			t.Errorf("query = %#v, want %#v", got, wantQuery)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(` {"seq":"9","type":"future.event","occurred_at":"2026-08-24T00:00:00Z","unknown":{"x":1}} `))
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"--url", srv.URL, "event", "stream", "--cursor", "8", "--mentions", "--tag", "api", "--tag", "design", "--max-events", "1"}
	code := runAPIContext(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	if code != apiOK || stdout.String() != `{"seq":"9","type":"future.event","occurred_at":"2026-08-24T00:00:00Z","unknown":{"x":1}}`+"\n" {
		t.Fatalf("exit=%d stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestAPIDestructiveNonInteractiveRequiresYes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAPIContext(context.Background(), []string{"--url", "http://127.0.0.1:1", "message", "delete", "id"}, strings.NewReader(""), &stdout, &stderr)
	if code != apiUsageExit || !strings.Contains(stderr.String(), "requires --yes") {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
}

func TestAPILocalValidationUsesUsageExit(t *testing.T) {
	tests := [][]string{
		{"--url", "http://127.0.0.1:1", "user", "list", "--limit", "0"},
		{"--url", "http://127.0.0.1:1", "event", "poll", "--timeout", "61"},
		{"--url", "http://127.0.0.1:1", "event", "stream", "--max-events", "0"},
		{"--url", "http://127.0.0.1:1", "thread", "create", "--title", "x", "--content", "a", "--content-file", "b"},
		{"--url", "http://127.0.0.1:1", "thread", "mark-read", "id"},
		{"--url", "http://127.0.0.1:1", "message", "delete", "id"},
		{"--url", "http://127.0.0.1:1", "--anonymous", "user", "list"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := runAPIContext(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != apiUsageExit {
			t.Errorf("%v: exit=%d stderr=%s", args, code, stderr.String())
		}
	}
}

func TestAPIDirectTokenFileOverridesDefaultEnvironmentSource(t *testing.T) {
	t.Setenv("ABBS_TOKEN", "wrong-env-token")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var targetAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/server" {
			_, _ = io.WriteString(w, `{"api_version":"v1","workspace":{"name":"test","visibility":"private","directory_listing":false},"auth_modes":["first-claim"],"limits":{}}`)
			return
		}
		targetAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"--url", srv.URL, "--token-file", tokenFile, "user", "list"}
	if code := runAPIContext(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != apiOK || targetAuth != "Bearer file-token" {
		t.Fatalf("exit=%d auth=%q stderr=%s", code, targetAuth, stderr.String())
	}
}

func TestAPIGeneratedIdempotencyKeyReportedAfterAmbiguousTransportFailure(t *testing.T) {
	t.Setenv("ABBS_TOKEN", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"api_version":"v1","workspace":{"name":"test","visibility":"private","directory_listing":false},"auth_modes":["first-claim"],"limits":{}}`)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"--url", srv.URL, "thread", "create", "--title", "test", "--content", "body"}
	code := runAPIContext(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	if code != apiIOExit {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	keyRE := regexp.MustCompile(`--idempotency-key [0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)
	if !keyRE.MatchString(stderr.String()) {
		t.Fatalf("generated key missing from stderr: %s", stderr.String())
	}
}

func TestAPILiveLifecycleAllImplementedOperations(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handler, err := abbsserver.New(st, abbsserver.Config{WorkspaceName: "cli-live"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	t.Setenv("ABBS_TOKEN", "")

	run := func(stdin string, command ...string) map[string]any {
		t.Helper()
		args := append([]string{"--url", srv.URL}, command...)
		var stdout, stderr bytes.Buffer
		if code := runAPIContext(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr); code != apiOK {
			t.Fatalf("%s: exit=%d stderr=%s", strings.Join(command, " "), code, stderr.String())
		}
		if stdout.Len() == 0 {
			return nil
		}
		var value map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
			t.Fatalf("%s: output %q: %v", strings.Join(command, " "), stdout.String(), err)
		}
		return value
	}

	// Discovery and the first-claim credential ceremony.
	run("", "server", "get")
	aliceClaim := run("", "user", "claim", "--username", "alice", "--kind", "human")
	aliceToken, _ := aliceClaim["token"].(string)
	if aliceToken == "" {
		t.Fatalf("claim response has no token: %#v", aliceClaim)
	}
	t.Setenv("ABBS_TOKEN", aliceToken)
	if err := st.SetAdmin("alice", true); err != nil {
		t.Fatal(err)
	}
	run("", "user", "claim", "--username", "bob", "--kind", "agent")
	run("", "user", "list", "--limit", "10")
	run("", "user", "get", "alice")

	created := run("", "thread", "create", "--title", "CLI lifecycle", "--content", "first", "--tag", "api")
	threadID, _ := created["id"].(string)
	if threadID == "" {
		t.Fatalf("thread response has no id: %#v", created)
	}
	run("", "thread", "list", "--tag", "api")
	run("", "thread", "get", threadID)
	run("", "thread", "set-tags", threadID, "--tag", "architecture")
	run("", "thread", "messages", threadID)
	reply := run("reply from stdin", "thread", "reply", threadID, "--content-file", "-")
	messageID, _ := reply["id"].(string)
	messageSeq, _ := reply["seq"].(string)
	if messageID == "" || messageSeq == "" {
		t.Fatalf("reply response incomplete: %#v", reply)
	}
	run("", "thread", "read-cursor", threadID)
	run("", "thread", "mark-read", threadID, "--seq", messageSeq)
	run("", "message", "get", messageID)
	run("", "message", "edit", messageID, "--content", "edited")
	run("", "reaction", "list", messageID)
	run("", "reaction", "add", messageID, "👍")
	run("", "reaction", "remove", messageID, "👍")
	run("", "event", "poll", "--limit", "10")
	run("", "event", "stream", "--max-events", "1")
	run("", "inbox", "list")
	run("", "tag", "list")
	run("", "tag", "subscription", "list")
	run("", "tag", "subscription", "add", "architecture")
	run("", "tag", "subscription", "remove", "architecture")
	run("", "message", "delete", messageID, "--yes")
	run("", "user", "deactivate", "bob", "--yes")
}
