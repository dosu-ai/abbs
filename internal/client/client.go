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
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dosu-ai/abbs/internal/api"
)

type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8080, no trailing slash
	Token   string // ABBS bearer token; empty for unauthenticated calls
	HTTP    *http.Client
}

// Error is a non-2xx response, carrying the server's RFC 9457 problem.
type Error struct {
	StatusCode int
	Problem    api.Problem
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

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		buf = bytes.NewReader(b)
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, buf)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		apiErr := &Error{StatusCode: resp.StatusCode}
		json.NewDecoder(resp.Body).Decode(&apiErr.Problem) // best effort
		return apiErr
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) ServerInfo(ctx context.Context) (api.ServerInfo, error) {
	var info api.ServerInfo
	err := c.do(ctx, "GET", "/v1/server", nil, nil, &info)
	return info, err
}

func (c *Client) ClaimUser(ctx context.Context, req api.ClaimUserRequest) (api.ClaimUserResponse, error) {
	var resp api.ClaimUserResponse
	err := c.do(ctx, "POST", "/v1/users", nil, req, &resp)
	return resp, err
}

func (c *Client) CreateThread(ctx context.Context, req api.CreateThreadRequest) (api.Thread, error) {
	var t api.Thread
	err := c.do(ctx, "POST", "/v1/threads", nil, req, &t)
	return t, err
}

func (c *Client) GetThread(ctx context.Context, threadID string) (api.Thread, error) {
	var t api.Thread
	err := c.do(ctx, "GET", "/v1/threads/"+url.PathEscape(threadID), nil, nil, &t)
	return t, err
}

type ListThreadsOptions struct {
	Since string
	Tags  []string
	Page  string
	Limit int
}

func (c *Client) ListThreads(ctx context.Context, opts ListThreadsOptions) (api.ThreadPage, error) {
	q := url.Values{}
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	for _, t := range opts.Tags {
		q.Add("tag", t)
	}
	if opts.Page != "" {
		q.Set("page", opts.Page)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	var page api.ThreadPage
	err := c.do(ctx, "GET", "/v1/threads", q, nil, &page)
	return page, err
}

func (c *Client) ListMessages(ctx context.Context, threadID, page string, limit int) (api.MessagePage, error) {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var mp api.MessagePage
	err := c.do(ctx, "GET", "/v1/threads/"+url.PathEscape(threadID)+"/messages", q, nil, &mp)
	return mp, err
}

func (c *Client) PostMessage(ctx context.Context, threadID, content string) (api.Message, error) {
	var m api.Message
	err := c.do(ctx, "POST", "/v1/threads/"+url.PathEscape(threadID)+"/messages", nil, api.CreateMessageRequest{Content: content}, &m)
	return m, err
}

func (c *Client) Inbox(ctx context.Context, page string, limit int) (api.InboxPage, error) {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var ip api.InboxPage
	err := c.do(ctx, "GET", "/v1/inbox", q, nil, &ip)
	return ip, err
}

func (c *Client) SetReadCursor(ctx context.Context, threadID, seq string) error {
	return c.do(ctx, "PUT", "/v1/threads/"+url.PathEscape(threadID)+"/read-cursor", nil, api.SetReadCursorRequest{Seq: seq}, nil)
}

func (c *Client) EditMessage(ctx context.Context, messageID, content string) (api.Message, error) {
	var m api.Message
	err := c.do(ctx, "PATCH", "/v1/messages/"+url.PathEscape(messageID), nil, api.EditMessageRequest{Content: content}, &m)
	return m, err
}

// DeleteMessage tombstones a message; the returned Message is the tombstone.
func (c *Client) DeleteMessage(ctx context.Context, messageID string) (api.Message, error) {
	var m api.Message
	err := c.do(ctx, "DELETE", "/v1/messages/"+url.PathEscape(messageID), nil, nil, &m)
	return m, err
}

func (c *Client) ListReactions(ctx context.Context, messageID, page string, limit int) (api.ReactionPage, error) {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var rp api.ReactionPage
	err := c.do(ctx, "GET", "/v1/messages/"+url.PathEscape(messageID)+"/reactions", q, nil, &rp)
	return rp, err
}

func (c *Client) ListTags(ctx context.Context, page string, limit int) (api.TagPage, error) {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var tp api.TagPage
	err := c.do(ctx, "GET", "/v1/tags", q, nil, &tp)
	return tp, err
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

// Events is the catch-up read / long-poll — the sync protocol the read
// cache replays.
func (c *Client) Events(ctx context.Context, opts EventsOptions) (api.EventBatch, error) {
	q := url.Values{}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	q.Set("timeout", strconv.Itoa(opts.TimeoutSeconds))
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
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
	for _, t := range opts.Tags {
		q.Add("tag", t)
	}
	var batch api.EventBatch
	err := c.do(ctx, "GET", "/v1/events", q, nil, &batch)
	return batch, err
}
