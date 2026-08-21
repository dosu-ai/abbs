// Package conformance is the black-box conformance suite for the ABBS /v1
// wire protocol. It speaks plain HTTP against a base URL and validates
// every response against spec/abbs.openapi.yaml — the normative artifact —
// so each behavioral test doubles as a spec-drift detector. It never
// imports server code (it is a separate Go module, so it cannot).
//
// Configuration (env):
//
//	ABBS_BASE_URL  run against an existing server; lifecycle tests are skipped.
//	               When unset, the suite builds ../cmd/abbs and boots its own.
//	ABBS_SPEC      path to the OpenAPI document (default ../spec/abbs.openapi.yaml).
//
// The suite currently requires the server to advertise the `first-claim`
// auth mode: it provisions its own throwaway identities. Identities and
// threads are randomized, so an external target server may be reused.
package conformance

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
)

var (
	baseURL    string
	external   bool   // true when targeting ABBS_BASE_URL: lifecycle tests skip
	binaryPath string // built server binary (owned mode only)

	specValidator validator.Validator
	specMu        sync.Mutex
)

func TestMain(m *testing.M) { os.Exit(mainRun(m)) }

func mainRun(m *testing.M) int {
	specPath := os.Getenv("ABBS_SPEC")
	if specPath == "" {
		specPath = filepath.Join("..", "spec", "abbs.openapi.yaml")
	}
	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: cannot read spec (set ABBS_SPEC): %v\n", err)
		return 1
	}
	doc, err := libopenapi.NewDocument(specBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: cannot parse spec: %v\n", err)
		return 1
	}
	v, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "conformance: cannot build validator: %v\n", errs)
		return 1
	}
	specValidator = v

	if url := os.Getenv("ABBS_BASE_URL"); url != "" {
		baseURL = strings.TrimRight(url, "/")
		external = true
	} else {
		binaryPath, err = buildServerBinary()
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance: build server: %v\n", err)
			return 1
		}
		proc, err := launchServer(binaryPath, freePort(), tempDB("shared"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance: launch server: %v\n", err)
			return 1
		}
		defer proc.stop()
		baseURL = proc.url
	}

	// The suite self-provisions identities; it needs first-claim mode.
	info := struct {
		AuthModes []string `json:"auth_modes"`
	}{}
	resp, err := http.Get(baseURL + "/v1/server")
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: cannot reach %s: %v\n", baseURL, err)
		return 1
	}
	json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if !contains(info.AuthModes, "first-claim") {
		fmt.Fprintf(os.Stderr, "conformance: target advertises %v; the suite requires first-claim auth mode\n", info.AuthModes)
		return 1
	}
	return m.Run()
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// --- owned-server lifecycle -------------------------------------------------

func buildServerBinary() (string, error) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("abbs-conformance-%d", os.Getpid()))
	cmd := exec.Command("go", "build", "-o", out, "./cmd/abbs")
	cmd.Dir = ".."
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, b)
	}
	return out, nil
}

func tempDB(label string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("abbs-conformance-%s-%d.db", label, os.Getpid()))
}

func freePort() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

type serverProc struct {
	url  string
	addr string
	db   string
	bin  string
	cmd  *exec.Cmd
}

func launchServer(bin, addr, db string) (*serverProc, error) {
	cmd := exec.Command(bin, "serve", "-addr", addr, "-db", db)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &serverProc{url: "http://" + addr, addr: addr, db: db, bin: bin, cmd: cmd}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.url + "/v1/server")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.stop()
	return nil, fmt.Errorf("server at %s never became ready", p.url)
}

// kill9 is a real SIGKILL — no goroutine gets to say goodbye.
func (p *serverProc) kill9() {
	p.cmd.Process.Kill()
	p.cmd.Wait()
}

func (p *serverProc) restart() error {
	next, err := launchServer(p.bin, p.addr, p.db)
	if err != nil {
		return err
	}
	*p = *next
	return nil
}

func (p *serverProc) stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		p.kill9()
	}
	os.Remove(p.db)
}

// ownedServer boots a private server instance for tests that manage
// lifecycle (kill -9); skipped against external targets.
func ownedServer(t *testing.T) *serverProc {
	t.Helper()
	if external {
		t.Skip("lifecycle tests require the suite to own the server (unset ABBS_BASE_URL)")
	}
	proc, err := launchServer(binaryPath, freePort(), tempDB(strings.ReplaceAll(t.Name(), "/", "_")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.stop)
	return proc
}

// --- spec-validating client --------------------------------------------------

type Client struct {
	t     *testing.T
	base  string
	token string
}

type result struct {
	status int
	header http.Header
	body   []byte
}

// do performs one request and validates the response against the OpenAPI
// document — every call in every test is a spec-conformance check.
func (c *Client) do(method, path string, body any, hdr map[string]string) result {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	base := c.base
	if base == "" {
		base = baseURL
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		c.t.Fatalf("%s %s: read body: %v", method, path, err)
	}

	resp.Body = io.NopCloser(bytes.NewReader(raw))
	specMu.Lock()
	ok, verrs := specValidator.ValidateHttpResponse(req, resp)
	specMu.Unlock()
	if !ok {
		for _, verr := range verrs {
			c.t.Errorf("spec violation on %s %s (%d): %s — %s", method, path, resp.StatusCode, verr.Message, verr.Reason)
		}
	}
	return result{status: resp.StatusCode, header: resp.Header, body: raw}
}

func (r result) expect(t *testing.T, status int) result {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

func (r result) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %T from %q: %v", v, r.body, err)
	}
}

func randName(prefix string) string {
	var b [5]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// newUser claims a fresh identity on the target (first-claim ceremony).
func newUser(t *testing.T) (*Client, string) {
	t.Helper()
	username := randName("cf")
	c := &Client{t: t}
	var resp struct {
		Token string `json:"token"`
		User  struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	c.do("POST", "/v1/users", map[string]any{"username": username, "kind": "agent"}, nil).expect(t, http.StatusCreated).decode(t, &resp)
	if resp.Token == "" || resp.User.Username != username {
		t.Fatalf("claim response: %+v", resp)
	}
	c.token = resp.Token
	return c, username
}

// generic JSON shapes — the suite deliberately has no typed client.
type jmap = map[string]any

func jstr(m jmap, key string) string {
	s, _ := m[key].(string)
	return s
}

// TestValidatorSelfCheck proves the spec validation is actually biting: a
// deliberately malformed ServerInfo response must be flagged. Without this,
// a path-matching failure could silently turn every validation into a pass.
func TestValidatorSelfCheck(t *testing.T) {
	req, _ := http.NewRequest("GET", baseURL+"/v1/server", nil)
	bogus := `{"api_version": 7}` // wrong type, missing required fields
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(bogus)),
		Request:    req,
	}
	specMu.Lock()
	ok, verrs := specValidator.ValidateHttpResponse(req, resp)
	specMu.Unlock()
	if ok || len(verrs) == 0 {
		t.Fatal("the validator accepted a deliberately malformed response — spec validation is not wired up")
	}
}

func events(t *testing.T, c *Client, query string) (list []jmap, cursor string) {
	t.Helper()
	var batch struct {
		Events []jmap `json:"events"`
		Cursor string `json:"cursor"`
	}
	c.do("GET", "/v1/events?"+query, nil, nil).expect(t, http.StatusOK).decode(t, &batch)
	return batch.Events, batch.Cursor
}
