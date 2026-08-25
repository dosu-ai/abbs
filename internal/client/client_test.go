package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/coder/websocket"

	"github.com/dosu-ai/abbs/internal/api"
)

func TestCompleteTypedOperationMethods(t *testing.T) {
	type call func(*Client) error
	tests := []struct {
		name, method, path, auth, body, idempotency string
		query                                       url.Values
		call                                        call
	}{
		{"list users", http.MethodGet, "/v1/users", "Bearer abbs", "", "", url.Values{"page": {"p"}, "limit": {"2"}}, func(c *Client) error { _, err := c.ListUsers(context.Background(), "p", 2); return err }},
		{"deactivate user", http.MethodPost, "/v1/users/alice/deactivate", "Bearer abbs", "", "key", nil, func(c *Client) error {
			_, err := c.DeactivateUser(context.Background(), "alice", WithIdempotencyKey("key"))
			return err
		}},
		{"update tags", http.MethodPatch, "/v1/threads/thread%20id", "Bearer abbs", `{"tags":["api"]}`, "key", nil, func(c *Client) error {
			_, err := c.UpdateThreadTags(context.Background(), "thread id", []string{"api"}, WithIdempotencyKey("key"))
			return err
		}},
		{"get read cursor", http.MethodGet, "/v1/threads/thread%20id/read-cursor", "Bearer abbs", "", "", nil, func(c *Client) error { _, err := c.GetReadCursor(context.Background(), "thread id"); return err }},
		{"get message", http.MethodGet, "/v1/messages/message%20id", "Bearer abbs", "", "", nil, func(c *Client) error { _, err := c.GetMessage(context.Background(), "message id"); return err }},
		{"add reaction", http.MethodPut, "/v1/messages/message%20id/reactions/%F0%9F%91%8D", "Bearer abbs", "", "key", nil, func(c *Client) error {
			return c.AddReaction(context.Background(), "message id", "👍", WithIdempotencyKey("key"))
		}},
		{"remove reaction", http.MethodDelete, "/v1/messages/message%20id/reactions/%F0%9F%91%8D", "Bearer abbs", "", "key", nil, func(c *Client) error {
			return c.RemoveReaction(context.Background(), "message id", "👍", WithIdempotencyKey("key"))
		}},
		{"list subscriptions", http.MethodGet, "/v1/tag-subscriptions", "Bearer abbs", "", "", url.Values{"page": {"p"}, "limit": {"3"}}, func(c *Client) error { _, err := c.ListTagSubscriptions(context.Background(), "p", 3); return err }},
		{"subscribe", http.MethodPut, "/v1/tag-subscriptions/go%2Flang", "Bearer abbs", "", "key", nil, func(c *Client) error {
			return c.SubscribeTag(context.Background(), "go/lang", WithIdempotencyKey("key"))
		}},
		{"unsubscribe", http.MethodDelete, "/v1/tag-subscriptions/go%2Flang", "Bearer abbs", "", "key", nil, func(c *Client) error {
			return c.UnsubscribeTag(context.Background(), "go/lang", WithIdempotencyKey("key"))
		}},
		{"register agent", http.MethodPost, "/v1/agents", "Bearer idp", `{"username":"helper"}`, "key", nil, func(c *Client) error {
			_, err := c.RegisterAgent(context.Background(), api.RegisterAgentRequest{Username: "helper"}, "idp", WithIdempotencyKey("key"))
			return err
		}},
		{"list agents", http.MethodGet, "/v1/agents", "Bearer abbs", "", "", url.Values{"page": {"p"}, "limit": {"4"}}, func(c *Client) error { _, err := c.ListAgents(context.Background(), "p", 4); return err }},
		{"get agent", http.MethodGet, "/v1/agents/helper", "Bearer abbs", "", "", nil, func(c *Client) error { _, err := c.GetAgent(context.Background(), "helper"); return err }},
		{"revoke tokens", http.MethodDelete, "/v1/agents/helper/tokens", "Bearer abbs", "", "key", nil, func(c *Client) error {
			return c.RevokeAgentTokens(context.Background(), "helper", WithIdempotencyKey("key"))
		}},
		{"refresh", http.MethodPost, "/v1/tokens/refresh", "", `{"refresh_token":"refresh"}`, "", nil, func(c *Client) error { _, err := c.RefreshToken(context.Background(), "refresh"); return err }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.EscapedPath() != tc.path {
					t.Errorf("got %s %s, want %s %s", r.Method, r.URL.EscapedPath(), tc.method, tc.path)
				}
				if !reflect.DeepEqual(r.URL.Query(), tc.query) && !(len(r.URL.Query()) == 0 && len(tc.query) == 0) {
					t.Errorf("query = %#v, want %#v", r.URL.Query(), tc.query)
				}
				if got := r.Header.Get("Authorization"); got != tc.auth {
					t.Errorf("Authorization = %q, want %q", got, tc.auth)
				}
				if got := r.Header.Get("Idempotency-Key"); got != tc.idempotency {
					t.Errorf("Idempotency-Key = %q, want %q", got, tc.idempotency)
				}
				b, _ := io.ReadAll(r.Body)
				if tc.body == "" && len(b) != 0 {
					t.Errorf("unexpected body %s", b)
				} else if tc.body != "" && !equalJSON(b, []byte(tc.body)) {
					t.Errorf("body = %s, want %s", b, tc.body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer srv.Close()
			if err := tc.call(&Client{BaseURL: srv.URL, Token: "abbs"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDoRawPreservesAdditiveSuccessAndProblemFields(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, ` {"type":"urn:test","title":"Conflict","status":409,"unknown":{"x":1}} `)
			return
		}
		_, _ = io.WriteString(w, ` {"known":true,"unknown":{"x":1}} `)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	resp, err := c.DoRaw(context.Background(), http.MethodGet, "/v1/test", nil, nil)
	if err != nil || string(resp.Body) != ` {"known":true,"unknown":{"x":1}} ` {
		t.Fatalf("body=%q err=%v", resp.Body, err)
	}
	fail = true
	_, err = c.DoRaw(context.Background(), http.MethodGet, "/v1/test", nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || string(apiErr.Raw) != ` {"type":"urn:test","title":"Conflict","status":409,"unknown":{"x":1}} ` {
		t.Fatalf("error=%#v", err)
	}
}

func TestDoRawRejectsEmptySuccessExceptNoContent(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{http.StatusOK, true},
		{http.StatusCreated, true},
		{http.StatusNoContent, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			_, err := (&Client{BaseURL: srv.URL}).DoRaw(context.Background(), http.MethodGet, "/v1/test", nil, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestStreamEventsHandshakeProblemAndRawFrame(t *testing.T) {
	var reject bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"urn:auth","title":"No","status":401,"unknown":true}`)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(` {"seq":"1","type":"future","occurred_at":"2026-08-24T00:00:00Z","unknown":true} `))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "secret"}
	stream, err := c.StreamEvents(context.Background(), EventsOptions{Cursor: "0", Tags: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := stream.Next(context.Background())
	_ = stream.Close()
	if err != nil || string(raw) != ` {"seq":"1","type":"future","occurred_at":"2026-08-24T00:00:00Z","unknown":true} ` {
		t.Fatalf("raw=%q err=%v", raw, err)
	}
	reject = true
	_, err = c.StreamEvents(context.Background(), EventsOptions{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || !json.Valid(apiErr.Raw) {
		t.Fatalf("handshake error=%#v", err)
	}
}

func equalJSON(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}
