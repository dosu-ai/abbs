// Package server implements the /v1 HTTP surface over a store: the full M4
// SQLite + first-claim configuration — threads, messages (edits,
// tombstones), reactions, tags and subscriptions, inbox and read cursors,
// filtered events long-poll, idempotency keys, per-user rate limits, the
// reply-loop guard, and the admin role. OAuth-mode agents endpoints arrive
// in M10.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/store"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// Auth modes selectable at serve time. The seam selects exactly one mode;
// all modes converge on "bearer token → principal" (DESIGN.md).
const (
	AuthFirstClaim    = "first-claim" // anyone may claim an unclaimed name
	AuthAPIKey        = "api-key"     // admin-issued static keys; claiming is off
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
	maxLimiterBuckets = 16_384
)

// Config tunes a server instance. Zero values take documented defaults.
type Config struct {
	WorkspaceName             string
	WorkspaceDescription      string
	WorkspaceVisibility       string
	WorkspaceCanonicalURL     string
	WorkspaceDirectoryListing bool
	// TrustedProxyCIDRs enables X-Forwarded-For only when the TCP peer is in
	// one of these networks. Leave empty when the server is directly exposed.
	TrustedProxyCIDRs []string

	// AuthMode selects the credential ceremony: AuthFirstClaim (default)
	// or AuthAPIKey.
	AuthMode string

	// Per-user write rate limit: token bucket.
	WriteBurst        int     // default 60
	WriteRefillPerSec float64 // default 1

	// Anonymous GET rate limit. These are test seams; operators use the
	// protocol defaults so public workspaces have one consistent budget.
	AnonymousBurst        int     // default 60
	AnonymousRefillPerSec float64 // default 1

	// Reply-loop guard: posting is rejected when the thread's last
	// LoopGuardMessages messages plus this one are authored by ≤2 distinct
	// users within LoopGuardWindow — the two-agents-ping-ponging shape.
	LoopGuardMessages int           // default 10
	LoopGuardWindow   time.Duration // default 2m
}

func (c Config) withDefaults() (Config, error) {
	if c.WorkspaceName == "" {
		c.WorkspaceName = "abbs"
	}
	if c.WorkspaceVisibility == "" {
		c.WorkspaceVisibility = VisibilityPrivate
	}
	if c.AuthMode == "" {
		c.AuthMode = AuthFirstClaim
	}
	if c.WriteBurst == 0 {
		c.WriteBurst = 60
	}
	if c.WriteRefillPerSec == 0 {
		c.WriteRefillPerSec = 1
	}
	if c.AnonymousBurst == 0 {
		c.AnonymousBurst = 60
	}
	if c.AnonymousRefillPerSec == 0 {
		c.AnonymousRefillPerSec = 1
	}
	if c.LoopGuardMessages == 0 {
		c.LoopGuardMessages = 10
	}
	if c.LoopGuardWindow == 0 {
		c.LoopGuardWindow = 2 * time.Minute
	}
	if n := utf8.RuneCountInString(c.WorkspaceName); n < 1 || n > 100 {
		return Config{}, fmt.Errorf("workspace name must be 1..100 Unicode code points")
	}
	if utf8.RuneCountInString(c.WorkspaceDescription) > 1000 {
		return Config{}, fmt.Errorf("workspace description must be at most 1000 Unicode code points")
	}
	if c.WorkspaceVisibility != VisibilityPrivate && c.WorkspaceVisibility != VisibilityPublic {
		return Config{}, fmt.Errorf("workspace visibility must be %q or %q", VisibilityPrivate, VisibilityPublic)
	}
	if c.AuthMode != AuthFirstClaim && c.AuthMode != AuthAPIKey {
		return Config{}, fmt.Errorf("auth mode must be %q or %q", AuthFirstClaim, AuthAPIKey)
	}
	if c.WorkspaceCanonicalURL != "" {
		if err := validateCanonicalOrigin(c.WorkspaceCanonicalURL); err != nil {
			return Config{}, fmt.Errorf("workspace canonical URL: %w", err)
		}
	}
	if c.WorkspaceVisibility == VisibilityPublic && c.WorkspaceCanonicalURL == "" {
		return Config{}, errors.New("public workspace requires a canonical URL")
	}
	if c.WorkspaceDirectoryListing {
		if c.WorkspaceVisibility != VisibilityPublic {
			return Config{}, errors.New("directory listing requires public workspace visibility")
		}
		if c.WorkspaceDescription == "" {
			return Config{}, errors.New("directory listing requires a non-empty workspace description")
		}
	}
	if c.WriteBurst < 1 || c.WriteRefillPerSec <= 0 || c.AnonymousBurst < 1 || c.AnonymousRefillPerSec <= 0 {
		return Config{}, errors.New("rate-limit burst and refill values must be positive")
	}
	return c, nil
}

func validateCanonicalOrigin(raw string) error {
	// net/url normalizes schemes to lowercase, so enforce the wire contract's
	// literal lowercase prefix before parsing.
	if !strings.HasPrefix(raw, "https://") {
		return errors.New("must be a valid HTTPS origin")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a valid HTTPS origin")
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("must be an HTTPS origin with no credentials, path, query, or fragment")
	}
	return nil
}

func parseTrustedProxyCIDRs(raw []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(raw))
	for _, cidr := range raw {
		cidr = strings.TrimSpace(cidr)
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q must be an IP CIDR", cidr)
		}
		out = append(out, network)
	}
	return out, nil
}

type Server struct {
	store            *store.Store
	cfg              Config
	info             api.ServerInfo
	limits           api.Limits
	limiter          *limiter
	anonymousLimiter *limiter
	claimLimiter     *limiter
	trustedProxies   []*net.IPNet
	idemLocks        sync.Map // (principal, endpoint, key) -> *sync.Mutex
}

func New(st *store.Store, cfg Config) (http.Handler, error) {
	var err error
	cfg, err = cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	trustedProxies, err := parseTrustedProxyCIDRs(cfg.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	limits := api.DefaultLimits()
	var description, canonicalURL *string
	if cfg.WorkspaceDescription != "" {
		description = &cfg.WorkspaceDescription
	}
	if cfg.WorkspaceCanonicalURL != "" {
		canonicalURL = &cfg.WorkspaceCanonicalURL
	}
	s := &Server{
		store:            st,
		cfg:              cfg,
		limits:           limits,
		limiter:          newLimiter(cfg.WriteBurst, cfg.WriteRefillPerSec, maxLimiterBuckets),
		anonymousLimiter: newLimiter(cfg.AnonymousBurst, cfg.AnonymousRefillPerSec, maxLimiterBuckets),
		claimLimiter:     newLimiter(3, 1.0/300, maxLimiterBuckets),
		trustedProxies:   trustedProxies,
		info: api.ServerInfo{
			APIVersion: "v1",
			Workspace: api.Workspace{
				Name: cfg.WorkspaceName, Description: description,
				Visibility: cfg.WorkspaceVisibility, CanonicalURL: canonicalURL,
				DirectoryListing: cfg.WorkspaceDirectoryListing,
			},
			AuthModes:    []string{cfg.AuthMode},
			Capabilities: []string{"websocket"},
			Limits:       limits,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/server", s.handleGetServer)
	mux.HandleFunc("POST /v1/users", s.write("POST /v1/users", s.handleClaimUser))
	mux.HandleFunc("GET /v1/users", s.handleListUsers)
	mux.HandleFunc("GET /v1/users/{username}", s.handleGetUser)
	mux.HandleFunc("POST /v1/users/{username}/deactivate", s.write("POST /v1/users/{username}/deactivate", s.handleDeactivateUser))
	mux.HandleFunc("POST /v1/threads", s.write("POST /v1/threads", s.handleCreateThread))
	mux.HandleFunc("GET /v1/threads", s.handleListThreads)
	mux.HandleFunc("GET /v1/threads/{thread_id}", s.handleGetThread)
	mux.HandleFunc("PATCH /v1/threads/{thread_id}", s.write("PATCH /v1/threads/{thread_id}", s.handleUpdateThreadTags))
	mux.HandleFunc("GET /v1/threads/{thread_id}/messages", s.handleListMessages)
	mux.HandleFunc("POST /v1/threads/{thread_id}/messages", s.write("POST /v1/threads/{thread_id}/messages", s.handlePostMessage))
	mux.HandleFunc("GET /v1/threads/{thread_id}/read-cursor", s.handleGetReadCursor)
	mux.HandleFunc("PUT /v1/threads/{thread_id}/read-cursor", s.write("PUT /v1/threads/{thread_id}/read-cursor", s.handleSetReadCursor))
	mux.HandleFunc("GET /v1/messages/{message_id}", s.handleGetMessage)
	mux.HandleFunc("PATCH /v1/messages/{message_id}", s.write("PATCH /v1/messages/{message_id}", s.handleEditMessage))
	mux.HandleFunc("DELETE /v1/messages/{message_id}", s.write("DELETE /v1/messages/{message_id}", s.handleDeleteMessage))
	mux.HandleFunc("GET /v1/messages/{message_id}/reactions", s.handleListReactions)
	mux.HandleFunc("PUT /v1/messages/{message_id}/reactions/{emoji}", s.write("PUT /v1/messages/{message_id}/reactions/{emoji}", s.handleAddReaction))
	mux.HandleFunc("DELETE /v1/messages/{message_id}/reactions/{emoji}", s.write("DELETE /v1/messages/{message_id}/reactions/{emoji}", s.handleRemoveReaction))
	mux.HandleFunc("GET /v1/tags", s.handleListTags)
	mux.HandleFunc("GET /v1/tag-subscriptions", s.handleListTagSubscriptions)
	mux.HandleFunc("PUT /v1/tag-subscriptions/{tag}", s.write("PUT /v1/tag-subscriptions/{tag}", s.handleSubscribeTag))
	mux.HandleFunc("DELETE /v1/tag-subscriptions/{tag}", s.write("DELETE /v1/tag-subscriptions/{tag}", s.handleUnsubscribeTag))
	mux.HandleFunc("GET /v1/inbox", s.handleInbox)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/events/ws", s.handleEventsWS)

	// Unmatched routes get a problem+json 404, not the mux's plain text.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			writeProblem(w, http.StatusNotFound, "not-found", "no such endpoint")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	}), nil
}

// MustNew is a convenience for tests and embedded fixtures whose static
// configuration is part of the source. Operator-controlled configuration
// must use New and surface its error before serving.
func MustNew(st *store.Store, cfg Config) http.Handler {
	h, err := New(st, cfg)
	if err != nil {
		panic(err)
	}
	return h
}

// NewToken mints an opaque bearer token and its storage hash. Tokens are
// random strings stored hashed; introspection is a DB lookup, revocation is
// immediate. Exported for the abbs admin CLI, which mints API keys against
// the database directly.
func NewToken() (token, tokenHash string) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	token = "abbs_" + base64.RawURLEncoding.EncodeToString(raw[:])
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func bearerToken(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token, ok && token != ""
}

// authenticate resolves the bearer token to a principal, writing the 401
// problem itself when it fails.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (api.User, bool) {
	token, ok := bearerToken(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return api.User{}, false
	}
	user, err := s.store.UserByTokenHash(hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "unknown token")
		return api.User{}, false
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return api.User{}, false
	}
	if user.Deactivated {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "user is deactivated")
		return api.User{}, false
	}
	return user, true
}

// conditionalReadViewer implements the public-workspace auth resolution for
// the five anonymous-capable reads. A supplied Authorization header is always
// authenticated (and rejected when malformed/unknown/deactivated); only a
// genuinely missing header may become an anonymous public viewer.
func (s *Server) conditionalReadViewer(w http.ResponseWriter, r *http.Request) (store.ReadViewer, *api.User, bool) {
	if len(r.Header.Values("Authorization")) > 0 {
		user, ok := s.authenticate(w, r)
		if !ok {
			return store.ReadViewer{}, nil, false
		}
		return store.AuthenticatedViewer(user.Username), &user, true
	}
	if s.cfg.WorkspaceVisibility != VisibilityPublic {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return store.ReadViewer{}, nil, false
	}
	if !s.allowAnonymous(w, r) {
		return store.ReadViewer{}, nil, false
	}
	return store.AnonymousViewer(), nil, true
}

func (s *Server) allowAnonymous(w http.ResponseWriter, r *http.Request) bool {
	if ok, retryAfter := s.anonymousLimiter.allow(s.anonymousClientKey(r), time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeProblem(w, http.StatusTooManyRequests, "rate-limited", "anonymous read rate limit")
		return false
	}
	return true
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(remoteAddr)
}

func ipInNetworks(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func addressKey(ip net.IP) string {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	if ip6 := ip.To16(); ip6 != nil {
		return ip6.Mask(net.CIDRMask(64, 128)).String()
	}
	return "anonymous:fallback"
}

// anonymousClientKey trusts X-Forwarded-For only across an explicitly
// configured proxy chain. Walking from the TCP peer toward the client avoids
// accepting attacker-supplied entries a well-behaved proxy appended to.
func (s *Server) anonymousClientKey(r *http.Request) string {
	peer := remoteIP(r.RemoteAddr)
	if peer == nil {
		return "anonymous:fallback"
	}
	client := peer
	if ipInNetworks(peer, s.trustedProxies) {
		var chain []string
		for _, value := range r.Header.Values("X-Forwarded-For") {
			chain = append(chain, strings.Split(value, ",")...)
		}
		for i := len(chain) - 1; i >= 0 && ipInNetworks(client, s.trustedProxies); i-- {
			next := net.ParseIP(strings.TrimSpace(chain[i]))
			if next == nil {
				return addressKey(peer)
			}
			client = next
		}
	}
	return addressKey(client)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	if !s.allowAnonymous(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.info)
}

func (s *Server) handleClaimUser(w http.ResponseWriter, r *http.Request) {
	// The endpoint is the credential ceremony for both modes: first-claim
	// lets anyone claim an unclaimed name; api-key mode turns it into
	// admin-issued key provisioning and rejects everyone else.
	if s.cfg.AuthMode != AuthFirstClaim {
		actor, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if !actor.Admin {
			writeProblem(w, http.StatusForbidden, "forbidden",
				"first-claim is disabled on this server; user creation requires an admin API key")
			return
		}
	}
	var req api.ClaimUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !usernameRE.MatchString(req.Username) {
		writeProblem(w, http.StatusBadRequest, "validation", "username must match ^[a-z0-9][a-z0-9._-]{0,31}$")
		return
	}
	if req.Kind != "human" && req.Kind != "agent" {
		writeProblem(w, http.StatusBadRequest, "validation", `kind must be "human" or "agent"`)
		return
	}
	if req.DisplayName != nil && utf8.RuneCountInString(*req.DisplayName) > 100 {
		writeProblem(w, http.StatusBadRequest, "validation", "display_name over 100 characters")
		return
	}
	token, tokenHash := NewToken()
	user, err := s.store.ClaimUser(req.Username, req.Kind, req.DisplayName, tokenHash, time.Now())
	if errors.Is(err, store.ErrUsernameTaken) {
		writeProblem(w, http.StatusConflict, "username-taken", fmt.Sprintf("%q is already claimed", req.Username))
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, api.ClaimUserResponse{User: user, Token: token})
}

// normalizeTags lowercases, trims, drops empties, and dedupes, preserving
// first-seen order.
func normalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := []string{}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func (s *Server) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req api.CreateThreadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if n := utf8.RuneCountInString(req.Title); n == 0 || n > s.limits.TitleMaxChars {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("title must be 1..%d characters", s.limits.TitleMaxChars))
		return
	}
	if !s.checkContent(w, req.Content) {
		return
	}
	tags := normalizeTags(req.Tags)
	if len(tags) > s.limits.ThreadMaxTags {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("at most %d tags per thread", s.limits.ThreadMaxTags))
		return
	}
	for _, t := range tags {
		if utf8.RuneCountInString(t) > s.limits.TagMaxChars {
			writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("tag %q over %d characters", t, s.limits.TagMaxChars))
			return
		}
	}

	// Presence of participants makes the thread a DM; the set is
	// participants ∪ creator, deduplicated, and is fixed forever.
	var participants []string
	if req.Participants != nil {
		set := map[string]bool{user.Username: true}
		participants = []string{user.Username}
		for _, p := range req.Participants {
			if !usernameRE.MatchString(p) {
				writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("invalid participant username %q", p))
				return
			}
			if !set[p] {
				set[p] = true
				participants = append(participants, p)
			}
		}
		if len(participants) < 2 {
			writeProblem(w, http.StatusBadRequest, "validation", "a DM needs at least one participant besides the creator")
			return
		}
		if len(participants) > s.limits.DMMaxParticipants {
			writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("at most %d DM participants including the creator", s.limits.DMMaxParticipants))
			return
		}
	}

	thread, _, err := s.store.CreateThread(user.Username, req.Title, req.Content, tags, participants, time.Now())
	var unknown store.ErrUnknownParticipant
	if errors.As(err, &unknown) {
		writeProblem(w, http.StatusBadRequest, "validation", unknown.Error())
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, thread)
}

func (s *Server) checkContent(w http.ResponseWriter, content string) bool {
	n := utf8.RuneCountInString(content)
	if n == 0 {
		writeProblem(w, http.StatusBadRequest, "validation", "content must not be empty")
		return false
	}
	if n > s.limits.MessageMaxChars {
		// Rejected, never truncated — and a distinct code from validation.
		writeProblem(w, http.StatusUnprocessableEntity, "content-too-long",
			fmt.Sprintf("content is %d code points; the limit is %d", n, s.limits.MessageMaxChars))
		return false
	}
	return true
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	viewer, _, ok := s.conditionalReadViewer(w, r)
	if !ok {
		return
	}
	thread, err := s.store.GetThread(r.PathValue("thread_id"), viewer)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not-found", "no such thread")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req api.CreateMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.checkContent(w, req.Content) {
		return
	}
	threadID := r.PathValue("thread_id")

	// Reply-loop guard: the last N messages plus this one, authored by ≤2
	// distinct users, inside the window — the runaway agent-pair shape.
	// Rapid legitimate dialogs are distinguished by pace, not by content.
	authors, oldest, err := s.store.LastAuthors(threadID, s.cfg.LoopGuardMessages)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if len(authors) == s.cfg.LoopGuardMessages && time.Since(oldest) < s.cfg.LoopGuardWindow {
		distinct := map[string]bool{user.Username: true}
		for _, a := range authors {
			distinct[a] = true
		}
		if len(distinct) <= 2 {
			retry := int(s.cfg.LoopGuardWindow.Seconds() / 2)
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeProblem(w, http.StatusTooManyRequests, "loop-guard",
				"reply-loop guard: too many rapid messages between too few authors in this thread")
			return
		}
	}

	msg, err := s.store.PostMessage(threadID, user.Username, req.Content, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not-found", "no such thread")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	viewer, _, ok := s.conditionalReadViewer(w, r)
	if !ok {
		return
	}
	after, ok := parsePageAnchor(w, r)
	if !ok {
		return
	}
	limit, ok := s.parseLimit(w, r, 50)
	if !ok {
		return
	}
	items, nextPage, asOf, err := s.store.ListMessages(r.PathValue("thread_id"), viewer, after, limit)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not-found", "no such thread")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.MessagePage{Items: items, NextPage: nextPage, AsOf: asOf})
}

func (s *Server) parseLimit(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > s.limits.PageMaxLimit {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("limit must be 1..%d", s.limits.PageMaxLimit))
		return 0, false
	}
	return n, true
}

// parsePageAnchor turns an optional page token into a seq anchor (0 = none).
func parsePageAnchor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	p := r.URL.Query().Get("page")
	if p == "" {
		return 0, true
	}
	n, err := store.ParseSeq(p)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid page token")
		return 0, false
	}
	return n, true
}

func (s *Server) handleListThreads(w http.ResponseWriter, r *http.Request) {
	viewer, _, ok := s.conditionalReadViewer(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	since := int64(0)
	if v := q.Get("since"); v != "" {
		n, err := store.ParseSeq(v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid since cursor")
			return
		}
		since = n
	}
	before, ok := parsePageAnchor(w, r)
	if !ok {
		return
	}
	limit, ok := s.parseLimit(w, r, 50)
	if !ok {
		return
	}
	tags := normalizeTags(q["tag"])
	if len(tags) > s.limits.ThreadMaxTags {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("at most %d tag filters", s.limits.ThreadMaxTags))
		return
	}
	for _, tag := range tags {
		if utf8.RuneCountInString(tag) > s.limits.TagMaxChars {
			writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("tag %q over %d characters", tag, s.limits.TagMaxChars))
			return
		}
	}
	items, nextPage, asOf, err := s.store.ListThreads(viewer, since, before, tags, limit)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.ThreadPage{Items: items, NextPage: nextPage, AsOf: asOf})
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	before, ok := parsePageAnchor(w, r)
	if !ok {
		return
	}
	limit, ok := s.parseLimit(w, r, 50)
	if !ok {
		return
	}
	items, nextPage, asOf, err := s.store.Inbox(user.Username, before, limit)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.InboxPage{Items: items, NextPage: nextPage, AsOf: asOf})
}

func (s *Server) handleGetReadCursor(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	seq, err := s.store.GetReadCursor(r.PathValue("thread_id"), user.Username)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not-found", "no such thread")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var rc api.ReadCursor
	if seq != nil {
		tok := strconv.FormatInt(*seq, 10)
		rc.Seq = &tok
	}
	writeJSON(w, http.StatusOK, rc)
}

func (s *Server) handleSetReadCursor(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req api.SetReadCursorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	seq, err := store.ParseSeq(req.Seq)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid seq")
		return
	}
	err = s.store.SetReadCursor(r.PathValue("thread_id"), user.Username, seq)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not-found", "no such thread")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents is the catch-up read and the long-poll — the same query,
// differing only in whether the server waits.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	query, ok := s.parseEventQuery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	after := query.after
	timeout := 0
	if t := q.Get("timeout"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > s.limits.PollMaxTimeoutSeconds {
			writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("timeout must be 0..%d seconds", s.limits.PollMaxTimeoutSeconds))
			return
		}
		timeout = n
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > s.limits.EventsMaxBatch {
			writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("limit must be 1..%d", s.limits.EventsMaxBatch))
			return
		}
		limit = n
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		// Grab the wakeup channel before querying: an append between the
		// query and the wait still wakes us, so no event can slip through.
		wakeup := s.store.Wakeup()
		events, cursor, err := s.store.Events(user.Username, after, limit, query.filter)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		remaining := time.Until(deadline)
		if len(events) > 0 || remaining <= 0 {
			// Empty batch echoes the request cursor — the client loop is
			// dumb and safe.
			writeJSON(w, http.StatusOK, api.EventBatch{Events: events, Cursor: cursor})
			return
		}
		select {
		case <-wakeup:
		case <-time.After(remaining):
		case <-r.Context().Done():
			return
		}
	}
}
