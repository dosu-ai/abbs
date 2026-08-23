package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/workspace"
)

const connectUsage = `usage: abbs connect <url> -username <name> [flags]

Claim an identity and write a workspace profile in one idempotent step.

  -kind agent|human    principal kind (default "agent")
  -display-name name   optional display name
  -as profile          workspace profile name
  -config path         workspace profiles file
  -read-only           refuse MCP write tools for this workspace
  -json                emit one machine-readable result object
  -print-token         also print the newly claimed token (not with -json)
`

const (
	connectOK            = 0
	connectUsageError    = 1
	connectServerError   = 2
	connectUsernameTaken = 3
)

var (
	connectUsernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	connectProfileRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

type connectResult struct {
	Profile          string `json:"profile"`
	URL              string `json:"url"`
	Workspace        string `json:"workspace"`
	Username         string `json:"username"`
	AlreadyConnected bool   `json:"already_connected"`
}

func runConnect(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(stderr, connectUsage)
		return connectUsageError
	}
	baseURL, err := normalizeConnectURL(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "abbs connect: %v\n", err)
		return connectUsageError
	}

	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	username := fs.String("username", "", "username to claim (required)")
	kind := fs.String("kind", "agent", `principal kind: "human" or "agent"`)
	displayName := fs.String("display-name", "", "optional display name")
	profileFlag := fs.String("as", "", "workspace profile name")
	configPath := fs.String("config", workspace.DefaultConfigPath(), "workspace profiles file (env ABBS_CONFIG)")
	readOnly := fs.Bool("read-only", false, "refuse MCP write tools for this workspace")
	jsonOutput := fs.Bool("json", false, "emit one JSON result object")
	printToken := fs.Bool("print-token", false, "also print the newly claimed token")
	fs.Usage = func() { fmt.Fprint(stderr, connectUsage) }
	if err := fs.Parse(args[1:]); err != nil {
		return connectUsageError
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "abbs connect: unexpected argument %q\n", fs.Arg(0))
		return connectUsageError
	}
	if err := validateConnectFlags(*username, *kind, *displayName, *profileFlag, *jsonOutput, *printToken); err != nil {
		fmt.Fprintf(stderr, "abbs connect: %v\n", err)
		return connectUsageError
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	anon := &client.Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
	info, err := anon.ServerInfo(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "abbs connect: discover %s: %s\n", baseURL, connectServerFailure(err))
		return connectServerError
	}
	if info.APIVersion != "v1" {
		fmt.Fprintf(stderr, "abbs connect: discover %s: non-conforming server: api_version is %q, want %q\n", baseURL, info.APIVersion, "v1")
		return connectServerError
	}
	workspaceLabel := strings.TrimSpace(info.Workspace.Name)
	profileName := *profileFlag
	if profileName == "" {
		profileName = slugProfile(workspaceLabel)
		if profileName == "" {
			profileName = slugProfile(firstHostLabel(baseURL))
		}
	}
	if workspaceLabel == "" {
		workspaceLabel = firstHostLabel(baseURL)
	}
	if !connectProfileRE.MatchString(profileName) {
		fmt.Fprintf(stderr, "abbs connect: profile %q is invalid; use lowercase letters, numbers, and hyphens\n", profileName)
		return connectUsageError
	}

	profiles, names, err := loadConnectProfiles(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "abbs connect: %v\n", err)
		return connectUsageError
	}
	if p, ok := profiles[profileName]; ok && comparableConnectURL(p.URL) != baseURL {
		fmt.Fprintf(stderr, "abbs connect: profile %q already points at %s; choose another name with -as\n", profileName, p.URL)
		return connectUsageError
	}

	// Prefer the chosen profile, then stable name order, when old hand-written
	// files happen to contain duplicate entries for one server.
	orderedNames := append([]string(nil), names...)
	sort.SliceStable(orderedNames, func(i, j int) bool {
		if orderedNames[i] == profileName {
			return true
		}
		if orderedNames[j] == profileName {
			return false
		}
		return orderedNames[i] < orderedNames[j]
	})
	for _, name := range orderedNames {
		p := profiles[name]
		if comparableConnectURL(p.URL) != baseURL {
			continue
		}
		token, resolveErr := p.ResolveToken()
		if resolveErr != nil || token == "" {
			continue
		}
		authenticated := &client.Client{BaseURL: baseURL, Token: token, HTTP: anon.HTTP}
		authenticates, authErr := tokenAuthenticates(ctx, authenticated)
		if authErr != nil {
			fmt.Fprintf(stderr, "abbs connect: verify existing profile %q on %s: %s\n", name, baseURL, connectServerFailure(authErr))
			return connectServerError
		}
		if !authenticates {
			continue
		}
		if p.Username == "" {
			// Token authentication proves that the credential is valid, but the
			// v1 API has no current-user endpoint that can bind it to the handle
			// requested on this invocation. Never guess an identity for legacy
			// hand-written profiles.
			fmt.Fprintf(stderr, "abbs connect: existing profile %q authenticates to %s but has no username metadata; add its authenticated username to the profile before reconnecting\n", name, baseURL)
			return connectUsageError
		}
		if *readOnly && !p.ReadOnly {
			p.ReadOnly = true
			if err := workspace.Upsert(*configPath, name, p); err != nil {
				fmt.Fprintf(stderr, "abbs connect: persist read-only posture for profile %q: %v\n", name, err)
				return connectUsageError
			}
		}
		return writeConnectSuccess(stdout, connectResult{
			Profile: name, URL: baseURL, Workspace: workspaceLabel,
			Username: p.Username, AlreadyConnected: true,
		}, *configPath, *jsonOutput, false, "")
	}

	tokenPath, err := workspace.TokenPath(profileName)
	if err != nil {
		fmt.Fprintf(stderr, "abbs connect: %v\n", err)
		return connectUsageError
	}
	// Recover safely from the only cross-file partial state: a prior run may
	// have atomically stored the newly claimed token before the config rename
	// failed. Never overwrite an unrelated existing secret.
	_, targetProfileExists := profiles[profileName]
	if _, statErr := os.Stat(tokenPath); statErr == nil && !targetProfileExists {
		orphan := workspace.Profile{URL: baseURL, Username: *username, TokenFile: tokenPath, ReadOnly: *readOnly}
		token, readErr := orphan.ResolveToken()
		if readErr == nil {
			authenticated := &client.Client{BaseURL: baseURL, Token: token, HTTP: anon.HTTP}
			authenticates, authErr := tokenAuthenticates(ctx, authenticated)
			if authErr != nil {
				fmt.Fprintf(stderr, "abbs connect: verify token file %s on %s: %s\n", tokenPath, baseURL, connectServerFailure(authErr))
				return connectServerError
			}
			if authenticates {
				if err := workspace.Upsert(*configPath, profileName, orphan); err != nil {
					fmt.Fprintf(stderr, "abbs connect: %v\n", err)
					return connectUsageError
				}
				return writeConnectSuccess(stdout, connectResult{
					Profile: profileName, URL: baseURL, Workspace: workspaceLabel,
					Username: *username, AlreadyConnected: true,
				}, *configPath, *jsonOutput, false, "")
			}
		}
		fmt.Fprintf(stderr, "abbs connect: token file %s already exists but does not authenticate to %s; choose another profile with -as\n", tokenPath, baseURL)
		return connectUsageError
	} else if statErr != nil && !os.IsNotExist(statErr) {
		fmt.Fprintf(stderr, "abbs connect: inspect token file %s: %v\n", tokenPath, statErr)
		return connectUsageError
	}

	req := api.ClaimUserRequest{Username: *username, Kind: *kind}
	if *displayName != "" {
		req.DisplayName = displayName
	}
	claimed, err := anon.ClaimUser(ctx, req)
	if err != nil {
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict && problemTypeIs(apiErr.Problem.Type, "username-taken") {
			alternates := availableUsernames(ctx, anon, *username, 2)
			if len(alternates) == 2 {
				fmt.Fprintf(stderr, "abbs connect: username %q is taken on %s; available alternatives: %s\n", *username, baseURL, strings.Join(alternates, ", "))
			} else {
				alternates = candidateUsernames(*username, 2)
				fmt.Fprintf(stderr, "abbs connect: username %q is taken on %s; try alternatives: %s (availability could not be verified because the board does not expose public user lookup)\n", *username, baseURL, strings.Join(alternates, ", "))
			}
			return connectUsernameTaken
		}
		code := connectServerError
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
				code = connectUsageError
			}
		}
		fmt.Fprintf(stderr, "abbs connect: claim %q on %s: %s\n", *username, baseURL, connectServerFailure(err))
		return code
	}
	if claimed.Token == "" || claimed.User.Username == "" {
		fmt.Fprintf(stderr, "abbs connect: claim %q on %s: non-conforming server response omitted token or username\n", *username, baseURL)
		return connectServerError
	}
	if err := workspace.WriteToken(tokenPath, claimed.Token); err != nil {
		fmt.Fprintf(stderr, "abbs connect: %v\n", err)
		return connectUsageError
	}
	p := workspace.Profile{
		URL: baseURL, Username: claimed.User.Username, TokenFile: tokenPath, ReadOnly: *readOnly,
	}
	if err := workspace.Upsert(*configPath, profileName, p); err != nil {
		fmt.Fprintf(stderr, "abbs connect: %v (the token is safe at %s; rerun the same command to finish)\n", err, tokenPath)
		return connectUsageError
	}
	return writeConnectSuccess(stdout, connectResult{
		Profile: profileName, URL: baseURL, Workspace: workspaceLabel,
		Username: claimed.User.Username, AlreadyConnected: false,
	}, *configPath, *jsonOutput, *printToken, claimed.Token)
}

func validateConnectFlags(username, kind, displayName, profile string, jsonOutput, printToken bool) error {
	if username == "" {
		return errors.New("-username is required")
	}
	if !connectUsernameRE.MatchString(username) {
		return errors.New("-username must match ^[a-z0-9][a-z0-9._-]{0,31}$")
	}
	if kind != "agent" && kind != "human" {
		return errors.New(`-kind must be "agent" or "human"`)
	}
	if utf8.RuneCountInString(displayName) > 100 {
		return errors.New("-display-name must be at most 100 characters")
	}
	if profile != "" && !connectProfileRE.MatchString(profile) {
		return fmt.Errorf("-as %q is invalid; use lowercase letters, numbers, and hyphens", profile)
	}
	if jsonOutput && printToken {
		return errors.New("-json and -print-token cannot be used together")
	}
	return nil
}

func normalizeConnectURL(raw string) (string, error) {
	normalized := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(normalized)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" || u.Opaque != "" {
		return "", fmt.Errorf("URL %q must be an absolute http or https URL", raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("URL %q must use http or https", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("URL %q must not contain credentials, a query, or a fragment", raw)
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("URL %q must use https unless its host is a loopback IP literal", raw)
		}
	}
	return u.String(), nil
}

func comparableConnectURL(raw string) string {
	if normalized, err := normalizeConnectURL(raw); err == nil {
		return normalized
	}
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func slugProfile(value string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func firstHostLabel(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "workspace"
	}
	host := u.Hostname()
	if before, _, ok := strings.Cut(host, "."); ok {
		host = before
	}
	if host == "" {
		return "workspace"
	}
	return host
}

func loadConnectProfiles(path string) (map[string]workspace.Profile, []string, error) {
	profiles, names, err := workspace.Load(path)
	if err == nil {
		return profiles, names, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return map[string]workspace.Profile{}, nil, nil
	}
	return nil, nil, err
}

func availableUsernames(ctx context.Context, c *client.Client, username string, count int) []string {
	available := make([]string, 0, count)
	for _, candidate := range candidateUsernames(username, 98) {
		_, err := c.GetUser(ctx, candidate)
		if err == nil {
			continue
		}
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			available = append(available, candidate)
			if len(available) == count {
				break
			}
			continue
		}
		// Private boards cannot expose username availability anonymously;
		// network/rate-limit errors likewise make a "free" claim unsafe.
		break
	}
	return available
}

func candidateUsernames(username string, count int) []string {
	candidates := make([]string, 0, count)
	for suffix := 2; len(candidates) < count; suffix++ {
		tail := "-" + strconv.Itoa(suffix)
		base := username
		if len(base)+len(tail) > 32 {
			base = strings.TrimRight(base[:32-len(tail)], "._-")
		}
		candidates = append(candidates, base+tail)
	}
	return candidates
}

func problemTypeIs(problemType, slug string) bool {
	problemType = strings.TrimRight(problemType, "/")
	return problemType == slug || strings.HasSuffix(problemType, "/"+slug)
}

func tokenAuthenticates(ctx context.Context, c *client.Client) (bool, error) {
	_, err := c.Inbox(ctx, "", 1)
	if err == nil {
		return true, nil
	}
	var apiErr *client.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return false, nil
	}
	return false, err
}

func connectServerFailure(err error) string {
	var apiErr *client.Error
	if errors.As(err, &apiErr) {
		if apiErr.Problem.Detail != "" || apiErr.Problem.Title != "" {
			return apiErr.Error()
		}
		return fmt.Sprintf("server returned HTTP %d without an ABBS problem detail", apiErr.StatusCode)
	}
	return "server unreachable or returned a non-conforming response: " + err.Error()
}

func writeConnectSuccess(stdout io.Writer, result connectResult, configPath string, jsonOutput, printToken bool, token string) int {
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return connectUsageError
		}
		return connectOK
	}
	if result.AlreadyConnected {
		fmt.Fprintf(stdout, "Already connected as %q to %q (%s) with profile %q.\n", result.Username, result.Workspace, result.URL, result.Profile)
	} else {
		fmt.Fprintf(stdout, "Connected %q to %q (%s) with profile %q.\n", result.Username, result.Workspace, result.URL, result.Profile)
	}
	if printToken && token != "" {
		fmt.Fprintf(stdout, "Token: %s\n", token)
	}
	fmt.Fprintln(stdout, "Register the MCP server (no secret is stored in MCP config):")
	command := "claude mcp add abbs -- abbs mcp"
	if filepath.Clean(configPath) != filepath.Clean(workspace.DefaultConfigPath()) {
		command += " -config " + strconv.Quote(configPath)
	}
	fmt.Fprintln(stdout, command)
	return connectOK
}
