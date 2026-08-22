package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/dosu-ai/abbs/internal/store"
)

const (
	webSocketWriteTimeout = 10 * time.Second
	webSocketPingInterval = 30 * time.Second
)

type parsedEventQuery struct {
	after  int64
	filter store.EventFilter
}

// parseEventQuery owns the cursor and filter parsing shared by long-poll and
// WebSocket transports, so their validation and filtering cannot drift.
func (s *Server) parseEventQuery(w http.ResponseWriter, r *http.Request) (parsedEventQuery, bool) {
	q := r.URL.Query()
	parsed := parsedEventQuery{}

	if cursors, present := q["cursor"]; present {
		if len(cursors) != 1 || cursors[0] == "" {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid cursor")
			return parsedEventQuery{}, false
		}
		after, err := store.ParseSeq(cursors[0])
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid cursor")
			return parsedEventQuery{}, false
		}
		parsed.after = after
	}

	var ok bool
	parsed.filter.Mentions, ok = parseEventFilterBool(w, q, "mentions")
	if !ok {
		return parsedEventQuery{}, false
	}
	parsed.filter.DMs, ok = parseEventFilterBool(w, q, "dms")
	if !ok {
		return parsedEventQuery{}, false
	}
	parsed.filter.SubscribedTags, ok = parseEventFilterBool(w, q, "subscribed_tags")
	if !ok {
		return parsedEventQuery{}, false
	}

	rawTags := q["tag"]
	if len(rawTags) > s.limits.ThreadMaxTags {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("at most %d event tag filters", s.limits.ThreadMaxTags))
		return parsedEventQuery{}, false
	}
	for _, rawTag := range rawTags {
		tag := strings.ToLower(strings.TrimSpace(rawTag))
		if tag == "" {
			writeProblem(w, http.StatusBadRequest, "validation", "event tag filters must not be empty")
			return parsedEventQuery{}, false
		}
		if utf8.RuneCountInString(tag) > s.limits.TagMaxChars {
			writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("tag %q over %d characters", tag, s.limits.TagMaxChars))
			return parsedEventQuery{}, false
		}
	}
	parsed.filter.Tags = normalizeTags(rawTags)
	return parsed, true
}

func parseEventFilterBool(w http.ResponseWriter, q url.Values, name string) (bool, bool) {
	values, present := q[name]
	if !present {
		return false, true
	}
	if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
		writeProblem(w, http.StatusBadRequest, "validation", name+" must be true or false")
		return false, false
	}
	return values[0] == "true", true
}

func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	query, ok := s.parseEventQuery(w, r)
	if !ok {
		return
	}
	if !headerContainsToken(r.Header, "Upgrade", "websocket") {
		writeProblem(w, http.StatusBadRequest, "validation", "missing Upgrade: websocket header")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already written the handshake failure.
	}
	defer conn.CloseNow()

	// coder/websocket's CloseRead helper treats application data as a policy
	// violation. ABBS instead reserves client messages for additive evolution,
	// so consume and ignore them while still processing ping/pong and close.
	// The discard path streams rather than buffers, so disabling the library's
	// 32 KiB message limit does not turn ignored frames into an allocation risk.
	conn.SetReadLimit(-1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go discardWebSocketMessages(ctx, conn, cancel)

	token, _ := bearerToken(r) // authenticate above proved it is present.
	tokenHash := hashToken(token)
	after := query.after
	ticker := time.NewTicker(webSocketPingInterval)
	defer ticker.Stop()

	for {
		// Subscribe before querying. An append between the query and wait closes
		// this channel, preserving the long-poll's lost-wakeup guarantee.
		wakeup := s.store.Wakeup()
		events, cursor, err := s.store.Events(user.Username, after, s.limits.EventsMaxBatch, query.filter)
		if err != nil {
			closeWebSocket(conn, websocket.StatusInternalError, "event query failed")
			return
		}
		for _, event := range events {
			raw, err := json.Marshal(event)
			if err != nil {
				closeWebSocket(conn, websocket.StatusInternalError, "event encoding failed")
				return
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, webSocketWriteTimeout)
			err = conn.Write(writeCtx, websocket.MessageText, raw)
			writeCancel()
			if err != nil {
				closeWebSocket(conn, websocket.StatusInternalError, "event delivery failed")
				return
			}
		}
		if len(events) > 0 {
			after, err = store.ParseSeq(cursor)
			if err != nil {
				closeWebSocket(conn, websocket.StatusInternalError, "invalid event cursor")
				return
			}
		}

		authorized, err := s.webSocketPrincipalAuthorized(user.Username, tokenHash)
		if err != nil {
			closeWebSocket(conn, websocket.StatusInternalError, "principal check failed")
			return
		}
		if !authorized {
			closeWebSocket(conn, websocket.StatusPolicyViolation, "credentials revoked or user deactivated")
			return
		}

		if len(events) > 0 {
			continue
		}
		select {
		case <-wakeup:
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, webSocketWriteTimeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				closeWebSocket(conn, websocket.StatusInternalError, "ping failed")
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func discardWebSocketMessages(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	defer cancel()
	for {
		_, reader, err := conn.Reader(ctx)
		if err != nil {
			return
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return
		}
	}
}

func (s *Server) webSocketPrincipalAuthorized(username, tokenHash string) (bool, error) {
	user, err := s.store.UserByTokenHash(tokenHash)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return user.Username == username && !user.Deactivated, nil
}

func closeWebSocket(conn *websocket.Conn, code websocket.StatusCode, reason string) {
	_ = conn.Close(code, reason)
}

func headerContainsToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
