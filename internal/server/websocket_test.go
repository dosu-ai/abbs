package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dosu-ai/abbs/internal/api"
)

func dialTestWebSocket(t *testing.T, c *client, path string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, c.base+path, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil && resp.Body != nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("websocket dial %s: %v (%d: %s)", path, err, resp.StatusCode, body)
		}
		t.Fatalf("websocket dial %s: %v", path, err)
	}
	if resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		conn.CloseNow()
		t.Fatalf("websocket dial %s returned response %+v", path, resp)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func failedTestWebSocketDial(t *testing.T, c *client, path string, wantStatus int) api.Problem {
	t.Helper()
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, c.base+path, &websocket.DialOptions{HTTPHeader: header})
	if conn != nil {
		conn.CloseNow()
		t.Fatalf("websocket dial %s unexpectedly upgraded", path)
	}
	if err == nil || resp == nil {
		t.Fatalf("websocket dial %s = (%v, %+v), want HTTP %d", path, err, resp, wantStatus)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("websocket dial %s = %d, want %d: %s", path, resp.StatusCode, wantStatus, body)
	}
	var problem api.Problem
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode websocket handshake problem: %v", err)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("websocket handshake content-type = %q", resp.Header.Get("Content-Type"))
	}
	return problem
}

func readTestWebSocketEvent(t *testing.T, conn *websocket.Conn) api.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read websocket event: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("websocket frame type = %v, want text", typ)
	}
	var event api.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("decode websocket event: %v", err)
	}
	return event
}

func currentEventCursor(t *testing.T, c *client) string {
	t.Helper()
	var batch api.EventBatch
	c.do("GET", "/v1/events", nil, http.StatusOK, &batch)
	return batch.Cursor
}

func TestWebSocketCapabilityAndHandshakeProblems(t *testing.T) {
	ts, _ := newServer(t)
	alice := claim(t, ts.URL, "alice")
	anon := &client{t: t, base: ts.URL}

	var info api.ServerInfo
	anon.do("GET", "/v1/server", nil, http.StatusOK, &info)
	if !slicesContains(info.Capabilities, "websocket") {
		t.Fatalf("capabilities = %v, want websocket", info.Capabilities)
	}

	problem := failedTestWebSocketDial(t, anon, "/v1/events/ws?cursor=banana", http.StatusUnauthorized)
	if !strings.HasSuffix(problem.Type, "/unauthorized") {
		t.Fatalf("unauthorized problem type = %q", problem.Type)
	}
	problem = failedTestWebSocketDial(t, alice, "/v1/events/ws?cursor=banana", http.StatusBadRequest)
	if !strings.HasSuffix(problem.Type, "/validation") {
		t.Fatalf("bad cursor problem type = %q", problem.Type)
	}
	problem = failedTestWebSocketDial(t, alice, "/v1/events/ws?mentions=maybe", http.StatusBadRequest)
	if !strings.HasSuffix(problem.Type, "/validation") {
		t.Fatalf("bad filter problem type = %q", problem.Type)
	}
	problem = failedTestWebSocketDial(t, alice, "/v1/events/ws?tag=", http.StatusBadRequest)
	if !strings.HasSuffix(problem.Type, "/validation") {
		t.Fatalf("empty tag filter problem type = %q", problem.Type)
	}

	// Poll and WebSocket use the same parser, including filter validation.
	alice.do("GET", "/v1/events?mentions=maybe", nil, http.StatusBadRequest, nil)
	var plainProblem api.Problem
	alice.do("GET", "/v1/events/ws", nil, http.StatusBadRequest, &plainProblem)
	if !strings.HasSuffix(plainProblem.Type, "/validation") || !strings.Contains(plainProblem.Detail, "Upgrade") {
		t.Fatalf("plain GET problem = %+v", plainProblem)
	}
}

func TestWebSocketLiveTailIgnoresClientFrames(t *testing.T) {
	ts, _ := newServer(t)
	alice := claim(t, ts.URL, "alice")
	bob := claim(t, ts.URL, "bob")
	cursor := currentEventCursor(t, bob)
	conn := dialTestWebSocket(t, bob, "/v1/events/ws?cursor="+url.QueryEscape(cursor))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unknown := []byte(`{"future_client_message":"` + strings.Repeat("x", 40<<10) + `"}`)
	if err := conn.Write(ctx, websocket.MessageText, unknown); err != nil {
		t.Fatalf("write unknown client frame: %v", err)
	}
	// Give the server's read loop a chance to consume the frame before the
	// append; a CloseRead implementation would close 1008 here.
	time.Sleep(25 * time.Millisecond)

	var thread api.Thread
	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "live", Content: "tail me"}, http.StatusCreated, &thread)
	event := readTestWebSocketEvent(t, conn)
	if event["type"] != "thread.created" {
		t.Fatalf("live websocket event = %+v", event)
	}
	gotThread, _ := event["thread"].(map[string]any)
	if gotThread["id"] != thread.ID {
		t.Fatalf("live websocket thread = %+v, want %s", gotThread, thread.ID)
	}
}

func TestWebSocketSubscribeBeforeQueryNoLostWakeup(t *testing.T) {
	ts, _ := newServer(t)
	alice := claim(t, ts.URL, "alice")
	bob := claim(t, ts.URL, "bob")
	cursor := currentEventCursor(t, bob)
	conn := dialTestWebSocket(t, bob, "/v1/events/ws?cursor="+url.QueryEscape(cursor))

	alice.do("POST", "/v1/threads", api.CreateThreadRequest{Title: "wakeup-0", Content: "0"}, http.StatusCreated, nil)
	lastSeq := ""
	for i := 0; i < 20; i++ {
		event := readTestWebSocketEvent(t, conn)
		if event["seq"] == lastSeq {
			t.Fatalf("duplicate websocket cursor %q", lastSeq)
		}
		lastSeq, _ = event["seq"].(string)
		if i == 19 {
			break
		}
		// Posting immediately after each delivery repeatedly exercises the
		// handler's transition from query back to its parked wakeup wait.
		alice.do("POST", "/v1/threads", api.CreateThreadRequest{
			Title: fmt.Sprintf("wakeup-%d", i+1), Content: "next",
		}, http.StatusCreated, nil)
	}
}

func TestWebSocketFiltersMatchPoll(t *testing.T) {
	ts, _ := newServer(t)
	writer := claim(t, ts.URL, "writer")
	reader := claim(t, ts.URL, "reader")
	claim(t, ts.URL, "outsider")
	tag := "websocket-filter"
	reader.do("PUT", "/v1/tag-subscriptions/"+tag, nil, http.StatusNoContent, nil)
	start := currentEventCursor(t, reader)

	writer.do("POST", "/v1/threads", api.CreateThreadRequest{
		Title: "tagged mention", Content: "hello @reader", Tags: []string{tag},
	}, http.StatusCreated, nil)
	writer.do("POST", "/v1/threads", api.CreateThreadRequest{
		Title: "control", Content: "public but unmatched",
	}, http.StatusCreated, nil)
	writer.do("POST", "/v1/threads", api.CreateThreadRequest{
		Title: "visible dm", Content: "private", Participants: []string{"reader"},
	}, http.StatusCreated, nil)
	writer.do("POST", "/v1/threads", api.CreateThreadRequest{
		Title: "hidden dm", Content: "secret @reader", Participants: []string{"outsider"},
	}, http.StatusCreated, nil)

	tests := []struct {
		name  string
		query string
	}{
		{name: "mentions", query: "mentions=true"},
		{name: "dms", query: "dms=true"},
		{name: "subscribed tags", query: "subscribed_tags=true"},
		{name: "explicit tag", query: "tag=" + url.QueryEscape(tag)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var poll api.EventBatch
			reader.do("GET", "/v1/events?cursor="+url.QueryEscape(start)+"&limit=100&"+tc.query,
				nil, http.StatusOK, &poll)
			if len(poll.Events) == 0 {
				t.Fatalf("poll returned no %s events", tc.name)
			}
			conn := dialTestWebSocket(t, reader, "/v1/events/ws?cursor="+url.QueryEscape(start)+"&"+tc.query)
			got := make([]api.Event, 0, len(poll.Events))
			for range poll.Events {
				got = append(got, readTestWebSocketEvent(t, conn))
			}
			if !reflect.DeepEqual(got, poll.Events) {
				t.Fatalf("websocket/poll %s mismatch\nwebsocket: %+v\npoll: %+v", tc.name, got, poll.Events)
			}
		})
	}
}

func TestWebSocketDeactivationClosesPolicyViolation(t *testing.T) {
	ts, st := newServer(t)
	admin := claim(t, ts.URL, "admin")
	victim := claim(t, ts.URL, "victim")
	if err := st.SetAdmin("admin", true); err != nil {
		t.Fatal(err)
	}
	cursor := currentEventCursor(t, victim)
	conn := dialTestWebSocket(t, victim, "/v1/events/ws?cursor="+url.QueryEscape(cursor))

	admin.do("POST", "/v1/users/victim/deactivate", nil, http.StatusOK, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // the deactivation event itself may precede the close.
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
			t.Fatalf("deactivation close status = %d, want %d: %v", got, websocket.StatusPolicyViolation, err)
		}
		break
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
