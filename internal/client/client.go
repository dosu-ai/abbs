// Package client is a thin Go client for the /v1 wire protocol — the MCP
// adapter is built on it. It consumes the public API exactly like any other
// client; there is no private side door.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/dosu-ai/abbs/internal/api"
)

const maxResponseBytes = 16 << 20

var defaultHTTPClient = &http.Client{Timeout: 70 * time.Second}

type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8080, no trailing slash
	Token   string // ABBS bearer token; empty for unauthenticated calls
	HTTP    *http.Client
}

// Error is a non-2xx response, carrying both the decoded RFC 9457 problem and
// its original JSON bytes. Raw is useful to CLIs that must preserve additive
// problem fields unknown to this package.
type Error struct {
	StatusCode int
	Problem    api.Problem
	Raw        json.RawMessage
}

func (e *Error) Error() string {
	if e.Problem.Title != "" {
		if e.Problem.Detail != "" {
			return fmt.Sprintf("%s: %s", e.Problem.Title, e.Problem.Detail)
		}
		return e.Problem.Title
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

type requestOptions struct {
	idempotencyKey string
	bearerToken    *string
}

// RequestOption changes one request without mutating shared Client state.
type RequestOption func(*requestOptions)

func WithIdempotencyKey(key string) RequestOption {
	return func(o *requestOptions) { o.idempotencyKey = key }
}

// WithBearerToken overrides the client's bearer for one request. Passing an
// empty string explicitly suppresses the client's bearer.
func WithBearerToken(token string) RequestOption {
	return func(o *requestOptions) { o.bearerToken = &token }
}

// RawResponse is the exact successful HTTP response body. Keeping it as bytes
// lets callers preserve unknown additive JSON fields.
type RawResponse struct {
	StatusCode int
	Header     http.Header
	Body       json.RawMessage
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

func applyOptions(options []RequestOption) requestOptions {
	var opts requestOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	return opts
}

func buildQuery(opts EventsOptions, stream bool) url.Values {
	q := url.Values{}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if !stream {
		q.Set("timeout", strconv.Itoa(opts.TimeoutSeconds))
		if opts.Limit > 0 {
			q.Set("limit", strconv.Itoa(opts.Limit))
		}
	}
	if opts.Mentions {
		q.Set("mentions", "true")
	}
	if opts.DMs {
		q.Set("dms", "true")
	}
	if opts.SubscribedTags {
		q.Set("subscribed_tags", "true")
	}
	for _, tag := range opts.Tags {
		q.Add("tag", tag)
	}
	return q
}

func (c *Client) requestURL(path string, query url.Values) string {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	return b, nil
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && (mediaType == "application/json" || mediaType == "application/problem+json" || strings.HasSuffix(mediaType, "+json"))
}

// DoRaw performs one request and returns the successful response without a
// typed decode/re-encode cycle. Non-2xx responses are returned as *Error.
func (c *Client) DoRaw(ctx context.Context, method, path string, query url.Values, body any, options ...RequestOption) (RawResponse, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return RawResponse{}, err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.requestURL(path, query), buf)
	if err != nil {
		return RawResponse{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	opts := applyOptions(options)
	token := c.Token
	if opts.bearerToken != nil {
		token = *opts.bearerToken
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if opts.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", opts.idempotencyKey)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return RawResponse{}, err
	}
	defer resp.Body.Close()
	b, err := readLimited(resp.Body)
	if err != nil {
		return RawResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &Error{StatusCode: resp.StatusCode, Raw: append(json.RawMessage(nil), b...)}
		_ = json.Unmarshal(b, &apiErr.Problem)
		return RawResponse{}, apiErr
	}
	if len(b) == 0 && resp.StatusCode != http.StatusNoContent {
		return RawResponse{}, fmt.Errorf("HTTP %d response has no JSON body", resp.StatusCode)
	}
	if len(b) > 0 {
		if !isJSONContentType(resp.Header.Get("Content-Type")) {
			return RawResponse{}, fmt.Errorf("HTTP %d response has non-JSON Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !json.Valid(b) {
			return RawResponse{}, fmt.Errorf("HTTP %d response contains malformed JSON", resp.StatusCode)
		}
	}
	return RawResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: append(json.RawMessage(nil), b...)}, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any, options ...RequestOption) error {
	resp, err := c.DoRaw(ctx, method, path, query, body, options...)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(resp.Body) == 0 {
		return fmt.Errorf("HTTP %d response has no JSON body", resp.StatusCode)
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decode HTTP %d response: %w", resp.StatusCode, err)
	}
	return nil
}

func (c *Client) ServerInfo(ctx context.Context) (api.ServerInfo, error) {
	var info api.ServerInfo
	err := c.do(ctx, http.MethodGet, "/v1/server", nil, nil, &info)
	return info, err
}

func (c *Client) ClaimUser(ctx context.Context, req api.ClaimUserRequest, options ...RequestOption) (api.ClaimUserResponse, error) {
	var resp api.ClaimUserResponse
	err := c.do(ctx, http.MethodPost, "/v1/users", nil, req, &resp, options...)
	return resp, err
}

func (c *Client) GetCurrentUser(ctx context.Context) (api.User, error) {
	var user api.User
	err := c.do(ctx, http.MethodGet, "/v1/me", nil, nil, &user)
	return user, err
}

func (c *Client) ListUsers(ctx context.Context, page string, limit int) (api.UserPage, error) {
	var out api.UserPage
	err := c.do(ctx, http.MethodGet, "/v1/users", pageQuery(page, limit), nil, &out)
	return out, err
}

// GetUser performs the exact-handle lookup. Public workspaces expose the
// minimal PublicUser shape anonymously; private workspaces require a token.
func (c *Client) GetUser(ctx context.Context, username string) (api.PublicUser, error) {
	var user api.PublicUser
	err := c.do(ctx, http.MethodGet, "/v1/users/"+url.PathEscape(username), nil, nil, &user)
	return user, err
}

func (c *Client) DeactivateUser(ctx context.Context, username string, options ...RequestOption) (api.User, error) {
	var out api.User
	err := c.do(ctx, http.MethodPost, "/v1/users/"+url.PathEscape(username)+"/deactivate", nil, nil, &out, options...)
	return out, err
}

func (c *Client) CreateThread(ctx context.Context, req api.CreateThreadRequest, options ...RequestOption) (api.Thread, error) {
	var out api.Thread
	err := c.do(ctx, http.MethodPost, "/v1/threads", nil, req, &out, options...)
	return out, err
}

func (c *Client) GetThread(ctx context.Context, threadID string) (api.Thread, error) {
	var out api.Thread
	err := c.do(ctx, http.MethodGet, "/v1/threads/"+url.PathEscape(threadID), nil, nil, &out)
	return out, err
}

type ListThreadsOptions struct {
	Since string
	Tags  []string
	Page  string
	Limit int
}

func (c *Client) ListThreads(ctx context.Context, opts ListThreadsOptions) (api.ThreadPage, error) {
	q := pageQuery(opts.Page, opts.Limit)
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	for _, tag := range opts.Tags {
		q.Add("tag", tag)
	}
	var out api.ThreadPage
	err := c.do(ctx, http.MethodGet, "/v1/threads", q, nil, &out)
	return out, err
}

func (c *Client) UpdateThreadTags(ctx context.Context, threadID string, tags []string, options ...RequestOption) (api.Thread, error) {
	var out api.Thread
	err := c.do(ctx, http.MethodPatch, "/v1/threads/"+url.PathEscape(threadID), nil, api.UpdateThreadRequest{Tags: tags}, &out, options...)
	return out, err
}

func pageQuery(page string, limit int) url.Values {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return q
}

func (c *Client) ListMessages(ctx context.Context, threadID, page string, limit int) (api.MessagePage, error) {
	var out api.MessagePage
	err := c.do(ctx, http.MethodGet, "/v1/threads/"+url.PathEscape(threadID)+"/messages", pageQuery(page, limit), nil, &out)
	return out, err
}

func (c *Client) PostMessage(ctx context.Context, threadID, content string, options ...RequestOption) (api.Message, error) {
	var out api.Message
	err := c.do(ctx, http.MethodPost, "/v1/threads/"+url.PathEscape(threadID)+"/messages", nil, api.CreateMessageRequest{Content: content}, &out, options...)
	return out, err
}

func (c *Client) GetReadCursor(ctx context.Context, threadID string) (api.ReadCursor, error) {
	var out api.ReadCursor
	err := c.do(ctx, http.MethodGet, "/v1/threads/"+url.PathEscape(threadID)+"/read-cursor", nil, nil, &out)
	return out, err
}

func (c *Client) Inbox(ctx context.Context, page string, limit int) (api.InboxPage, error) {
	var out api.InboxPage
	err := c.do(ctx, http.MethodGet, "/v1/inbox", pageQuery(page, limit), nil, &out)
	return out, err
}

func (c *Client) SetReadCursor(ctx context.Context, threadID, seq string, options ...RequestOption) error {
	return c.do(ctx, http.MethodPut, "/v1/threads/"+url.PathEscape(threadID)+"/read-cursor", nil, api.SetReadCursorRequest{Seq: seq}, nil, options...)
}

func (c *Client) GetMessage(ctx context.Context, messageID string) (api.Message, error) {
	var out api.Message
	err := c.do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(messageID), nil, nil, &out)
	return out, err
}

func (c *Client) EditMessage(ctx context.Context, messageID, content string, options ...RequestOption) (api.Message, error) {
	var out api.Message
	err := c.do(ctx, http.MethodPatch, "/v1/messages/"+url.PathEscape(messageID), nil, api.EditMessageRequest{Content: content}, &out, options...)
	return out, err
}

// DeleteMessage tombstones a message; the returned Message is the tombstone.
func (c *Client) DeleteMessage(ctx context.Context, messageID string, options ...RequestOption) (api.Message, error) {
	var out api.Message
	err := c.do(ctx, http.MethodDelete, "/v1/messages/"+url.PathEscape(messageID), nil, nil, &out, options...)
	return out, err
}

func (c *Client) ListReactions(ctx context.Context, messageID, page string, limit int) (api.ReactionPage, error) {
	var out api.ReactionPage
	err := c.do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(messageID)+"/reactions", pageQuery(page, limit), nil, &out)
	return out, err
}

func (c *Client) AddReaction(ctx context.Context, messageID, reaction string, options ...RequestOption) error {
	path := "/v1/messages/" + url.PathEscape(messageID) + "/reactions/" + url.PathEscape(reaction)
	return c.do(ctx, http.MethodPut, path, nil, nil, nil, options...)
}

func (c *Client) RemoveReaction(ctx context.Context, messageID, reaction string, options ...RequestOption) error {
	path := "/v1/messages/" + url.PathEscape(messageID) + "/reactions/" + url.PathEscape(reaction)
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil, options...)
}

func (c *Client) ListTags(ctx context.Context, page string, limit int) (api.TagPage, error) {
	var out api.TagPage
	err := c.do(ctx, http.MethodGet, "/v1/tags", pageQuery(page, limit), nil, &out)
	return out, err
}

func (c *Client) ListTagSubscriptions(ctx context.Context, page string, limit int) (api.TagSubscriptionPage, error) {
	var out api.TagSubscriptionPage
	err := c.do(ctx, http.MethodGet, "/v1/tag-subscriptions", pageQuery(page, limit), nil, &out)
	return out, err
}

func (c *Client) SubscribeTag(ctx context.Context, tag string, options ...RequestOption) error {
	return c.do(ctx, http.MethodPut, "/v1/tag-subscriptions/"+url.PathEscape(tag), nil, nil, nil, options...)
}

func (c *Client) UnsubscribeTag(ctx context.Context, tag string, options ...RequestOption) error {
	return c.do(ctx, http.MethodDelete, "/v1/tag-subscriptions/"+url.PathEscape(tag), nil, nil, nil, options...)
}

type EventsOptions struct {
	Cursor         string
	TimeoutSeconds int // 0 returns immediately
	Limit          int
	Mentions       bool
	DMs            bool
	SubscribedTags bool
	Tags           []string
}

// Events is the catch-up read / long-poll — the sync protocol the read cache replays.
func (c *Client) Events(ctx context.Context, opts EventsOptions) (api.EventBatch, error) {
	var out api.EventBatch
	err := c.do(ctx, http.MethodGet, "/v1/events", buildQuery(opts, false), nil, &out)
	return out, err
}

func (c *Client) RegisterAgent(ctx context.Context, req api.RegisterAgentRequest, idpToken string, options ...RequestOption) (api.RegisterAgentResponse, error) {
	var out api.RegisterAgentResponse
	options = append(options, WithBearerToken(idpToken))
	err := c.do(ctx, http.MethodPost, "/v1/agents", nil, req, &out, options...)
	return out, err
}

func (c *Client) ListAgents(ctx context.Context, page string, limit int) (api.UserPage, error) {
	var out api.UserPage
	err := c.do(ctx, http.MethodGet, "/v1/agents", pageQuery(page, limit), nil, &out)
	return out, err
}

func (c *Client) GetAgent(ctx context.Context, username string) (api.User, error) {
	var out api.User
	err := c.do(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(username), nil, nil, &out)
	return out, err
}

func (c *Client) RevokeAgentTokens(ctx context.Context, username string, options ...RequestOption) error {
	return c.do(ctx, http.MethodDelete, "/v1/agents/"+url.PathEscape(username)+"/tokens", nil, nil, nil, options...)
}

func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (api.TokenPair, error) {
	var out api.TokenPair
	err := c.do(ctx, http.MethodPost, "/v1/tokens/refresh", nil, api.RefreshTokenRequest{RefreshToken: refreshToken}, &out, WithBearerToken(""))
	return out, err
}

// EventStream yields one raw JSON event per WebSocket text frame.
type EventStream struct {
	conn *websocket.Conn
}

func (c *Client) StreamEvents(ctx context.Context, opts EventsOptions) (*EventStream, error) {
	header := http.Header{}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	conn, resp, err := websocket.Dial(ctx, c.requestURL("/v1/events/ws", buildQuery(opts, true)), &websocket.DialOptions{HTTPClient: hc, HTTPHeader: header})
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			b, readErr := readLimited(resp.Body)
			if readErr != nil {
				return nil, readErr
			}
			apiErr := &Error{StatusCode: resp.StatusCode, Raw: append(json.RawMessage(nil), b...)}
			_ = json.Unmarshal(b, &apiErr.Problem)
			return nil, apiErr
		}
		return nil, err
	}
	return &EventStream{conn: conn}, nil
}

func (s *EventStream) Next(ctx context.Context) (json.RawMessage, error) {
	typ, b, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("WebSocket event frame is not text")
	}
	if len(b) > maxResponseBytes {
		return nil, fmt.Errorf("WebSocket event frame exceeds %d bytes", maxResponseBytes)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("WebSocket event frame contains malformed JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(b, &object); err != nil || object == nil {
		return nil, fmt.Errorf("WebSocket event frame is not a JSON object")
	}
	for _, field := range []string{"seq", "type", "occurred_at"} {
		var value string
		raw, ok := object[field]
		if !ok || json.Unmarshal(raw, &value) != nil || value == "" {
			return nil, fmt.Errorf("WebSocket event frame has invalid or missing %q", field)
		}
	}
	return append(json.RawMessage(nil), b...), nil
}

func (s *EventStream) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}
