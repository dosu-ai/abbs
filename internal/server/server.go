// Package server implements the /v1 HTTP surface over a store. M2 scope:
// discovery, first-claim identity, create thread, post message, read
// thread, and the unfiltered events long-poll. The rest of the spec lands
// in M4.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/store"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

type Server struct {
	store  *store.Store
	info   api.ServerInfo
	limits api.Limits
}

func New(st *store.Store, workspaceName, workspaceDescription string) http.Handler {
	limits := api.DefaultLimits()
	s := &Server{
		store:  st,
		limits: limits,
		info: api.ServerInfo{
			APIVersion: "v1",
			Workspace:  api.Workspace{Name: workspaceName, Description: workspaceDescription},
			AuthModes:  []string{"first-claim"},
			Limits:     limits,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/server", s.handleGetServer)
	mux.HandleFunc("POST /v1/users", s.handleClaimUser)
	mux.HandleFunc("POST /v1/threads", s.handleCreateThread)
	mux.HandleFunc("GET /v1/threads", s.handleListThreads)
	mux.HandleFunc("GET /v1/threads/{thread_id}", s.handleGetThread)
	mux.HandleFunc("GET /v1/threads/{thread_id}/messages", s.handleListMessages)
	mux.HandleFunc("POST /v1/threads/{thread_id}/messages", s.handlePostMessage)
	mux.HandleFunc("GET /v1/threads/{thread_id}/read-cursor", s.handleGetReadCursor)
	mux.HandleFunc("PUT /v1/threads/{thread_id}/read-cursor", s.handleSetReadCursor)
	mux.HandleFunc("GET /v1/inbox", s.handleInbox)
	mux.HandleFunc("GET /v1/events", s.handleEvents)

	// Unmatched routes get a problem+json 404, not the mux's plain text.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			writeProblem(w, http.StatusNotFound, "not-found", "no such endpoint")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}

// newToken mints an opaque bearer token and its storage hash. Tokens are
// random strings stored hashed; introspection is a DB lookup, revocation is
// immediate.
func newToken() (token, tokenHash string) {
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

// authenticate resolves the bearer token to a principal, writing the 401
// problem itself when it fails.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (api.User, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
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

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.info)
}

func (s *Server) handleClaimUser(w http.ResponseWriter, r *http.Request) {
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
	token, tokenHash := newToken()
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
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	thread, err := s.store.GetThread(r.PathValue("thread_id"), user.Username)
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
	msg, err := s.store.PostMessage(r.PathValue("thread_id"), user.Username, req.Content, time.Now())
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
	user, ok := s.authenticate(w, r)
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
	items, nextPage, asOf, err := s.store.ListMessages(r.PathValue("thread_id"), user.Username, after, limit)
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
	user, ok := s.authenticate(w, r)
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
	items, nextPage, asOf, err := s.store.ListThreads(user.Username, since, before, tags, limit)
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
	q := r.URL.Query()

	after := int64(0)
	if c := q.Get("cursor"); c != "" {
		n, err := store.ParseSeq(c)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid cursor")
			return
		}
		after = n
	}
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
		events, cursor, err := s.store.Events(user.Username, after, limit)
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
