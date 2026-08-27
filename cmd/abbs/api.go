package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/google/uuid"

	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/workspace"
)

const (
	apiOK        = 0
	apiUsageExit = 1
	apiIOExit    = 2
	apiHTTPExit  = 3
)

type apiAuthMode uint8

const (
	apiNoBearer apiAuthMode = iota
	apiOptionalBearer
	apiBearer
	apiIDPBearer
	apiRefreshSecret
)

type apiRunFunc func(context.Context, *apiEnvironment, apiOperation, []string) int

type apiOperation struct {
	OperationID      string
	CommandPath      []string
	Usage            string
	Method           string
	PathTemplate     string
	SuccessStatus    int
	Auth             apiAuthMode
	Mutating         bool
	Destructive      bool
	AnonymousAllowed bool
	Run              apiRunFunc
}

// apiOperations is deliberately the single parity registry. The test suite
// compares its operation IDs with every /v1 operation in the normative spec.
var apiOperations = []apiOperation{
	{"getServer", []string{"server", "get"}, "server get", http.MethodGet, "/v1/server", http.StatusOK, apiNoBearer, false, false, true, runServerGet},
	{"claimUser", []string{"user", "claim"}, "user claim --username <name> [flags]", http.MethodPost, "/v1/users", http.StatusCreated, apiOptionalBearer, true, false, false, runUserClaim},
	{"getCurrentUser", []string{"user", "me"}, "user me", http.MethodGet, "/v1/me", http.StatusOK, apiBearer, false, false, false, runServerGet},
	{"listUsers", []string{"user", "list"}, "user list [flags]", http.MethodGet, "/v1/users", http.StatusOK, apiBearer, false, false, false, runPage},
	{"getUser", []string{"user", "get"}, "user get <username>", http.MethodGet, "/v1/users/{username}", http.StatusOK, apiBearer, false, false, true, runOnePathRead},
	{"deactivateUser", []string{"user", "deactivate"}, "user deactivate <username> [--yes]", http.MethodPost, "/v1/users/{username}/deactivate", http.StatusOK, apiBearer, true, true, false, runDestructive},
	{"createThread", []string{"thread", "create"}, "thread create --title <title> (--content <text> | --content-file <path>) [flags]", http.MethodPost, "/v1/threads", http.StatusCreated, apiBearer, true, false, false, runThreadCreate},
	{"listThreads", []string{"thread", "list"}, "thread list [flags]", http.MethodGet, "/v1/threads", http.StatusOK, apiBearer, false, false, true, runThreadList},
	{"getThread", []string{"thread", "get"}, "thread get <thread-id>", http.MethodGet, "/v1/threads/{thread_id}", http.StatusOK, apiBearer, false, false, true, runOnePathRead},
	{"updateThreadTags", []string{"thread", "set-tags"}, "thread set-tags <thread-id> [--tag <tag> ...]", http.MethodPatch, "/v1/threads/{thread_id}", http.StatusOK, apiBearer, true, false, false, runThreadSetTags},
	{"listMessages", []string{"thread", "messages"}, "thread messages <thread-id> [flags]", http.MethodGet, "/v1/threads/{thread_id}/messages", http.StatusOK, apiBearer, false, false, true, runPageWithPath},
	{"postMessage", []string{"thread", "reply"}, "thread reply <thread-id> (--content <text> | --content-file <path>)", http.MethodPost, "/v1/threads/{thread_id}/messages", http.StatusCreated, apiBearer, true, false, false, runMessageWrite},
	{"getReadCursor", []string{"thread", "read-cursor"}, "thread read-cursor <thread-id>", http.MethodGet, "/v1/threads/{thread_id}/read-cursor", http.StatusOK, apiBearer, false, false, false, runOnePathRead},
	{"setReadCursor", []string{"thread", "mark-read"}, "thread mark-read <thread-id> --seq <cursor>", http.MethodPut, "/v1/threads/{thread_id}/read-cursor", http.StatusNoContent, apiBearer, true, false, false, runMarkRead},
	{"getMessage", []string{"message", "get"}, "message get <message-id>", http.MethodGet, "/v1/messages/{message_id}", http.StatusOK, apiBearer, false, false, false, runOnePathRead},
	{"editMessage", []string{"message", "edit"}, "message edit <message-id> (--content <text> | --content-file <path>)", http.MethodPatch, "/v1/messages/{message_id}", http.StatusOK, apiBearer, true, false, false, runMessageWrite},
	{"deleteMessage", []string{"message", "delete"}, "message delete <message-id> [--yes]", http.MethodDelete, "/v1/messages/{message_id}", http.StatusOK, apiBearer, true, true, false, runDestructive},
	{"listReactions", []string{"reaction", "list"}, "reaction list <message-id> [flags]", http.MethodGet, "/v1/messages/{message_id}/reactions", http.StatusOK, apiBearer, false, false, false, runPageWithPath},
	{"addReaction", []string{"reaction", "add"}, "reaction add <message-id> <emoji>", http.MethodPut, "/v1/messages/{message_id}/reactions/{emoji}", http.StatusNoContent, apiBearer, true, false, false, runTwoPathMutation},
	{"removeReaction", []string{"reaction", "remove"}, "reaction remove <message-id> <emoji>", http.MethodDelete, "/v1/messages/{message_id}/reactions/{emoji}", http.StatusNoContent, apiBearer, true, false, false, runTwoPathMutation},
	{"pollEvents", []string{"event", "poll"}, "event poll [filters]", http.MethodGet, "/v1/events", http.StatusOK, apiBearer, false, false, false, runEventPoll},
	{"streamEventsWebSocket", []string{"event", "stream"}, "event stream [filters] [--max-events <n>]", http.MethodGet, "/v1/events/ws", http.StatusSwitchingProtocols, apiBearer, false, false, false, runEventStream},
	{"getInbox", []string{"inbox", "list"}, "inbox list [flags]", http.MethodGet, "/v1/inbox", http.StatusOK, apiBearer, false, false, false, runPage},
	{"listTags", []string{"tag", "list"}, "tag list [flags]", http.MethodGet, "/v1/tags", http.StatusOK, apiBearer, false, false, true, runPage},
	{"listTagSubscriptions", []string{"tag", "subscription", "list"}, "tag subscription list [flags]", http.MethodGet, "/v1/tag-subscriptions", http.StatusOK, apiBearer, false, false, false, runPage},
	{"subscribeTag", []string{"tag", "subscription", "add"}, "tag subscription add <tag>", http.MethodPut, "/v1/tag-subscriptions/{tag}", http.StatusNoContent, apiBearer, true, false, false, runOnePathMutation},
	{"unsubscribeTag", []string{"tag", "subscription", "remove"}, "tag subscription remove <tag>", http.MethodDelete, "/v1/tag-subscriptions/{tag}", http.StatusNoContent, apiBearer, true, false, false, runOnePathMutation},
	{"registerAgent", []string{"agent", "register"}, "agent register --username <name> (--idp-token-file <path> | --idp-token-env <name>)", http.MethodPost, "/v1/agents", http.StatusCreated, apiIDPBearer, true, false, false, runAgentRegister},
	{"listAgents", []string{"agent", "list"}, "agent list [flags]", http.MethodGet, "/v1/agents", http.StatusOK, apiBearer, false, false, false, runPage},
	{"getAgent", []string{"agent", "get"}, "agent get <username>", http.MethodGet, "/v1/agents/{username}", http.StatusOK, apiBearer, false, false, false, runOnePathRead},
	{"revokeAgentTokens", []string{"agent", "revoke-tokens"}, "agent revoke-tokens <username> [--yes]", http.MethodDelete, "/v1/agents/{username}/tokens", http.StatusNoContent, apiBearer, true, true, false, runDestructive},
	{"refreshToken", []string{"token", "refresh"}, "token refresh [--refresh-token-file <path> | --refresh-token-env <name>]", http.MethodPost, "/v1/tokens/refresh", http.StatusOK, apiRefreshSecret, true, false, false, runTokenRefresh},
}

const apiUsage = `usage: abbs api [global-flags] <resource> <action> [flags]

Target and output flags (place before the resource name):
  --workspace name    select a workspace profile (optional with exactly one)
  --config path       workspace profiles file (env ABBS_CONFIG)
  --url base-url      direct target; mutually exclusive with --workspace
  --token-file path   direct-target bearer credential
  --token-env name    direct-target bearer env var (default ABBS_TOKEN)
  --anonymous         suppress credentials on a public read
  --pretty            pretty-print HTTP JSON responses
  --json-errors       write raw RFC 9457 problem JSON to stderr

Commands:
  server get
  user claim | me | list | get <username> | deactivate <username>
  thread create | list | get <id> | set-tags <id> | messages <id>
  thread reply <id> | read-cursor <id> | mark-read <id>
  message get <id> | edit <id> | delete <id>
  reaction list <message-id> | add <message-id> <emoji> | remove <message-id> <emoji>
  event poll | stream
  inbox list
  tag list | subscription list | subscription add <tag> | subscription remove <tag>
  agent register | list | get <username> | revoke-tokens <username>
  token refresh
`

type trackedString struct {
	value string
	set   bool
}

func (v *trackedString) String() string { return v.value }
func (v *trackedString) Set(value string) error {
	v.value, v.set = value, true
	return nil
}

type apiGlobalFlags struct {
	workspace  string
	configPath string
	url        string
	tokenFile  trackedString
	tokenEnv   trackedString
	anonymous  bool
	pretty     bool
	jsonErrors bool
}

type apiEnvironment struct {
	global  apiGlobalFlags
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	secrets []string
}

func runAPI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAPIContext(ctx, args, stdin, stdout, stderr)
}

func runAPIContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	g := apiGlobalFlags{configPath: workspace.DefaultConfigPath(), tokenEnv: trackedString{value: "ABBS_TOKEN"}}
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&g.workspace, "workspace", "", "workspace profile")
	fs.StringVar(&g.configPath, "config", g.configPath, "workspace profiles file")
	fs.StringVar(&g.url, "url", "", "direct server base URL")
	fs.Var(&g.tokenFile, "token-file", "direct-target bearer token file")
	fs.Var(&g.tokenEnv, "token-env", "direct-target bearer token environment variable")
	fs.BoolVar(&g.anonymous, "anonymous", false, "suppress credentials for an anonymous public read")
	fs.BoolVar(&g.pretty, "pretty", false, "pretty-print JSON")
	fs.BoolVar(&g.jsonErrors, "json-errors", false, "write raw problem JSON")
	fs.Usage = func() { fmt.Fprint(stderr, apiUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return apiOK
		}
		return apiUsageExit
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, apiUsage)
		return apiUsageExit
	}
	op, commandArgs, ok := findAPIOperation(rest)
	if !ok {
		fmt.Fprintf(stderr, "abbs api: unknown command %q\n", strings.Join(rest, " "))
		fmt.Fprint(stderr, apiUsage)
		return apiUsageExit
	}
	if g.anonymous && !op.AnonymousAllowed {
		fmt.Fprintf(stderr, "abbs api %s: --anonymous is not allowed for this operation\n", strings.Join(op.CommandPath, " "))
		return apiUsageExit
	}
	if g.anonymous && (g.tokenFile.set || g.tokenEnv.set) {
		fmt.Fprintf(stderr, "abbs api %s: --anonymous cannot be combined with --token-file or --token-env\n", strings.Join(op.CommandPath, " "))
		return apiUsageExit
	}
	env := &apiEnvironment{global: g, stdin: stdin, stdout: stdout, stderr: stderr}
	return op.Run(ctx, env, op, commandArgs)
}

func findAPIOperation(args []string) (apiOperation, []string, bool) {
	indexes := make([]int, len(apiOperations))
	for i := range indexes {
		indexes[i] = i
	}
	sort.SliceStable(indexes, func(i, j int) bool {
		return len(apiOperations[indexes[i]].CommandPath) > len(apiOperations[indexes[j]].CommandPath)
	})
	for _, index := range indexes {
		op := apiOperations[index]
		if len(args) < len(op.CommandPath) {
			continue
		}
		matched := true
		for i := range op.CommandPath {
			if args[i] != op.CommandPath[i] {
				matched = false
				break
			}
		}
		if matched {
			return op, args[len(op.CommandPath):], true
		}
	}
	return apiOperation{}, nil, false
}

func (e *apiEnvironment) target(op apiOperation) (workspace.Target, error) {
	mode := workspace.CredentialNone
	switch op.Auth {
	case apiOptionalBearer:
		mode = workspace.CredentialOptional
	case apiBearer:
		if !e.global.anonymous {
			mode = workspace.CredentialRequired
		}
	}
	tokenFile, tokenEnv := "", ""
	if e.global.url != "" {
		tokenFile = e.global.tokenFile.value
		if !e.global.tokenFile.set || e.global.tokenEnv.set {
			tokenEnv = e.global.tokenEnv.value
		}
	} else if e.global.tokenFile.set || e.global.tokenEnv.set {
		return workspace.Target{}, fmt.Errorf("--token-file and --token-env require --url; profile credentials come from the workspace config")
	}
	return workspace.ResolveTarget(workspace.ResolveOptions{
		ConfigPath: e.global.configPath,
		Workspace:  e.global.workspace,
		URL:        e.global.url,
		TokenFile:  tokenFile,
		TokenEnv:   tokenEnv,
		Credential: mode,
		Mutating:   op.Mutating,
	})
}

func (e *apiEnvironment) discover(ctx context.Context, c *client.Client) (map[string]bool, int) {
	info, err := c.ServerInfo(ctx)
	if err != nil {
		return nil, e.writeRequestError("discovery", err, "")
	}
	if info.APIVersion != "v1" {
		fmt.Fprintf(e.stderr, "abbs api: malformed discovery response: api_version is %q, want %q\n", info.APIVersion, "v1")
		return nil, apiIOExit
	}
	capabilities := make(map[string]bool, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		capabilities[capability] = true
	}
	return capabilities, apiOK
}

type apiRequest struct {
	path           string
	query          url.Values
	body           any
	bearerOverride *string
	idempotencyKey string
}

func (e *apiEnvironment) executeHTTP(ctx context.Context, op apiOperation, request apiRequest) int {
	if len(request.idempotencyKey) > 128 {
		fmt.Fprintf(e.stderr, "abbs api %s: --idempotency-key must be at most 128 characters\n", strings.Join(op.CommandPath, " "))
		return apiUsageExit
	}
	target, err := e.target(op)
	if err != nil {
		fmt.Fprintf(e.stderr, "abbs api %s: %v\n", strings.Join(op.CommandPath, " "), err)
		return apiUsageExit
	}
	c := &client.Client{BaseURL: target.URL, Token: target.Token}
	e.addSecret(target.Token)
	if request.bearerOverride != nil {
		e.addSecret(*request.bearerOverride)
	}
	options := make([]client.RequestOption, 0, 2)
	generatedKey := ""
	if op.Mutating {
		key := request.idempotencyKey
		if key == "" {
			key = uuid.NewString()
			generatedKey = key
		}
		options = append(options, client.WithIdempotencyKey(key))
	}
	if request.bearerOverride != nil {
		options = append(options, client.WithBearerToken(*request.bearerOverride))
	}
	resp, err := c.DoRaw(ctx, op.Method, request.path, request.query, request.body, options...)
	if err != nil {
		return e.writeRequestError(strings.Join(op.CommandPath, " "), err, generatedKey)
	}
	if resp.StatusCode != op.SuccessStatus {
		return e.writeRequestError(strings.Join(op.CommandPath, " "), fmt.Errorf("malformed response: HTTP status %d, want %d", resp.StatusCode, op.SuccessStatus), generatedKey)
	}
	if len(resp.Body) == 0 {
		return apiOK
	}
	if err := writeJSON(e.stdout, resp.Body, e.global.pretty); err != nil {
		fmt.Fprintf(e.stderr, "abbs api %s: write output: %v\n", strings.Join(op.CommandPath, " "), err)
		return apiIOExit
	}
	return apiOK
}

func writeJSON(w io.Writer, raw []byte, pretty bool) error {
	var out bytes.Buffer
	var err error
	if pretty {
		err = json.Indent(&out, raw, "", "  ")
	} else {
		err = json.Compact(&out, raw)
	}
	if err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err = w.Write(out.Bytes())
	return err
}

func (e *apiEnvironment) writeRequestError(action string, err error, generatedKey string) int {
	var apiErr *client.Error
	if errors.As(err, &apiErr) {
		if e.global.jsonErrors && json.Valid(apiErr.Raw) {
			_ = writeJSON(e.stderr, e.redactProblemJSON(apiErr.Raw), false)
		} else {
			fmt.Fprintf(e.stderr, "abbs api %s: %s\n", action, e.redactText(apiErr.Error()))
		}
		return apiHTTPExit
	}
	if errors.Is(err, context.Canceled) {
		return apiOK
	}
	fmt.Fprintf(e.stderr, "abbs api %s: %s\n", action, e.redactText(err.Error()))
	if generatedKey != "" {
		fmt.Fprintf(e.stderr, "request may have committed; retry the same body with --idempotency-key %s\n", generatedKey)
	}
	return apiIOExit
}

func (e *apiEnvironment) addSecret(secret string) {
	if secret == "" {
		return
	}
	for _, existing := range e.secrets {
		if existing == secret {
			return
		}
	}
	e.secrets = append(e.secrets, secret)
}

func (e *apiEnvironment) redactText(value string) string {
	for _, secret := range e.secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func (e *apiEnvironment) redactProblemJSON(raw []byte) []byte {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return []byte(`{"type":"about:blank","title":"HTTP error","status":0,"detail":"response contained a redacted credential"}`)
	}
	if !jsonContainsSecret(value, e.secrets) {
		return raw
	}
	value = redactJSONStrings(value, e.secrets)
	redacted, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"type":"about:blank","title":"HTTP error","status":0,"detail":"response contained a redacted credential"}`)
	}
	return redacted
}

func jsonContainsSecret(value any, secrets []string) bool {
	contains := func(candidate string) bool {
		for _, secret := range secrets {
			if strings.Contains(candidate, secret) {
				return true
			}
		}
		return false
	}
	switch value := value.(type) {
	case string:
		return contains(value)
	case []any:
		for _, item := range value {
			if jsonContainsSecret(item, secrets) {
				return true
			}
		}
	case map[string]any:
		for key, item := range value {
			if contains(key) || jsonContainsSecret(item, secrets) {
				return true
			}
		}
	}
	return false
}

func redactJSONStrings(value any, secrets []string) any {
	switch value := value.(type) {
	case string:
		for _, secret := range secrets {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
		return value
	case []any:
		for i := range value {
			value[i] = redactJSONStrings(value[i], secrets)
		}
		return value
	case map[string]any:
		redacted := make(map[string]any, len(value))
		for key, item := range value {
			for _, secret := range secrets {
				key = strings.ReplaceAll(key, secret, "[REDACTED]")
			}
			redacted[key] = redactJSONStrings(item, secrets)
		}
		return redacted
	default:
		return value
	}
}
