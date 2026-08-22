package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func requireWebSocket(t *testing.T) {
	t.Helper()
	var info struct {
		Capabilities []string `json:"capabilities"`
	}
	(&Client{t: t}).do("GET", "/v1/server", nil, nil).expect(t, http.StatusOK).decode(t, &info)
	if !contains(info.Capabilities, "websocket") {
		t.Skip("target does not advertise the optional websocket capability")
	}
}

func webSocketDialOptions(c *Client) *websocket.DialOptions {
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	return &websocket.DialOptions{HTTPHeader: header}
}

func dialWebSocket(t *testing.T, c *Client, path string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, baseURL+path, webSocketDialOptions(c))
	if err != nil {
		if resp != nil {
			got := validateHTTPResponse(t, resp.Request, resp)
			t.Fatalf("websocket dial %s: %v (%d: %s)", path, err, got.status, got.body)
		}
		t.Fatalf("websocket dial %s: %v", path, err)
	}
	if resp == nil {
		conn.CloseNow()
		t.Fatalf("websocket dial %s returned no handshake response", path)
	}
	validateHTTPResponse(t, resp.Request, resp).expect(t, http.StatusSwitchingProtocols)
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func failedWebSocketDial(t *testing.T, c *Client, path string, wantStatus int) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, baseURL+path, webSocketDialOptions(c))
	if conn != nil {
		conn.CloseNow()
		t.Fatalf("websocket dial %s unexpectedly upgraded", path)
	}
	if err == nil {
		t.Fatalf("websocket dial %s failed without an error", path)
	}
	if resp == nil {
		t.Fatalf("websocket dial %s returned no HTTP problem response: %v", path, err)
	}
	return validateHTTPResponse(t, resp.Request, resp).expect(t, wantStatus)
}

func validateEventFrame(t *testing.T, raw []byte) jmap {
	t.Helper()
	specMu.Lock()
	ok, verrs := eventValidator.ValidateSchemaBytes(eventSchema, raw)
	specMu.Unlock()
	if !ok {
		t.Fatalf("websocket frame violates components.schemas.Event: %v\n%s", verrs, raw)
	}
	var event jmap
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("decode websocket event %q: %v", raw, err)
	}
	return event
}

func readWebSocketEvent(t *testing.T, conn *websocket.Conn) jmap {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	typ, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read websocket event: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("websocket message type %v, want text", typ)
	}
	return validateEventFrame(t, raw)
}

func readWebSocketEvents(t *testing.T, conn *websocket.Conn, n int) []jmap {
	t.Helper()
	got := make([]jmap, 0, n)
	for range n {
		got = append(got, readWebSocketEvent(t, conn))
	}
	return got
}

func drainEventCursor(t *testing.T, c *Client) string {
	t.Helper()
	q := url.Values{"limit": {"100"}}
	for {
		list, cursor := events(t, c, q.Encode())
		if len(list) == 0 {
			return cursor
		}
		q.Set("cursor", cursor)
	}
}

func pollEventsAfter(t *testing.T, c *Client, cursor string, filters url.Values) []jmap {
	t.Helper()
	q := url.Values{}
	for key, values := range filters {
		q[key] = append([]string(nil), values...)
	}
	q.Set("cursor", cursor)
	q.Set("limit", "100")
	var all []jmap
	for {
		list, next := events(t, c, q.Encode())
		if len(list) == 0 {
			return all
		}
		if next == cursor {
			t.Fatalf("non-empty event batch did not advance cursor %q", cursor)
		}
		all = append(all, list...)
		cursor = next
		q.Set("cursor", cursor)
	}
}

func webSocketPath(cursor string, filters url.Values) string {
	q := url.Values{}
	for key, values := range filters {
		q[key] = append([]string(nil), values...)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if encoded := q.Encode(); encoded != "" {
		return "/v1/events/ws?" + encoded
	}
	return "/v1/events/ws"
}

func assertEventSequencesEqual(t *testing.T, got, want []jmap) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("websocket returned %d events, poll returned %d\nwebsocket: %+v\npoll: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if jstr(got[i], "seq") != jstr(want[i], "seq") || jstr(got[i], "type") != jstr(want[i], "type") {
			t.Fatalf("event %d envelope differs: websocket {%s %s}, poll {%s %s}", i,
				jstr(got[i], "seq"), jstr(got[i], "type"), jstr(want[i], "seq"), jstr(want[i], "type"))
		}
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("event %d payload differs\nwebsocket: %+v\npoll: %+v", i, got[i], want[i])
		}
	}
}

func eventThreadID(event jmap) string {
	if thread, ok := event["thread"].(jmap); ok {
		return jstr(thread, "id")
	}
	if message, ok := event["message"].(jmap); ok {
		return jstr(message, "thread_id")
	}
	return jstr(event, "thread_id")
}

func assertProblem(t *testing.T, got result, slug string) {
	t.Helper()
	if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("content-type %q, want problem+json", ct)
	}
	if !strings.Contains(string(got.body), slug) {
		t.Errorf("problem %s: %s", slug, got.body)
	}
}

func TestEventFrameValidatorSelfCheck(t *testing.T) {
	validateEventFrame(t, []byte(`{"seq":"1","type":"future.event","occurred_at":"2025-01-01T00:00:00Z"}`))
	specMu.Lock()
	ok, _ := eventValidator.ValidateSchemaBytes(eventSchema, []byte(`{"type":"missing-seq","occurred_at":"2025-01-01T00:00:00Z"}`))
	specMu.Unlock()
	if ok {
		t.Fatal("the Event schema validator accepted a frame without seq")
	}
}

func TestWebSocketTailMatchesPoll(t *testing.T) {
	requireWebSocket(t)
	writer, _ := newUser(t)
	reader, readerName := newUser(t)
	_, hiddenParticipant := newUser(t)
	start := drainEventCursor(t, reader)
	conn := dialWebSocket(t, reader, webSocketPath(start, nil))

	tag := randName("wstag")
	var public jmap
	writer.do("POST", "/v1/threads", jmap{
		"title": randName("wsmixed"), "content": "hello @" + readerName, "tags": []string{tag},
	}, nil).expect(t, http.StatusCreated).decode(t, &public)
	publicID := jstr(public, "id")
	var reply jmap
	writer.do("POST", "/v1/threads/"+publicID+"/messages", jmap{"content": "draft"}, nil).
		expect(t, http.StatusCreated).decode(t, &reply)
	messageID := jstr(reply, "id")
	writer.do("PATCH", "/v1/messages/"+messageID, jmap{"content": "edited"}, nil).expect(t, http.StatusOK)
	reader.do("PUT", "/v1/messages/"+messageID+"/reactions/"+url.PathEscape("👍"), nil, nil).expect(t, http.StatusNoContent)
	reader.do("DELETE", "/v1/messages/"+messageID+"/reactions/"+url.PathEscape("👍"), nil, nil).expect(t, http.StatusNoContent)
	writer.do("PATCH", "/v1/threads/"+publicID, jmap{"tags": []string{tag, randName("added")}}, nil).expect(t, http.StatusOK)
	writer.do("DELETE", "/v1/messages/"+messageID, nil, nil).expect(t, http.StatusOK)

	var visibleDM jmap
	writer.do("POST", "/v1/threads", jmap{
		"title": randName("visible-dm"), "content": "private", "participants": []string{readerName},
	}, nil).expect(t, http.StatusCreated).decode(t, &visibleDM)
	var hiddenDM jmap
	writer.do("POST", "/v1/threads", jmap{
		"title": randName("hidden-dm"), "content": "must not leak", "participants": []string{hiddenParticipant},
	}, nil).expect(t, http.StatusCreated).decode(t, &hiddenDM)
	writer.do("POST", "/v1/threads/"+publicID+"/messages", jmap{"content": "after hidden DM"}, nil).expect(t, http.StatusCreated)

	want := pollEventsAfter(t, reader, start, nil)
	if len(want) == 0 {
		t.Fatal("mixed traffic produced no poll events")
	}
	types := map[string]bool{}
	sawVisibleDM := false
	for _, event := range want {
		types[jstr(event, "type")] = true
		switch eventThreadID(event) {
		case jstr(visibleDM, "id"):
			sawVisibleDM = true
		case jstr(hiddenDM, "id"):
			t.Fatalf("poll leaked a DM to a non-participant: %+v", event)
		}
	}
	for _, typ := range []string{"thread.created", "message.created", "message.edited", "message.deleted", "thread.tags_changed", "reaction.added", "reaction.removed"} {
		if !types[typ] {
			t.Errorf("mixed traffic did not produce %s", typ)
		}
	}
	if !sawVisibleDM {
		t.Fatal("poll missed the reader's DM")
	}

	got := readWebSocketEvents(t, conn, len(want))
	assertEventSequencesEqual(t, got, want)
}

func TestWebSocketReconnect(t *testing.T) {
	requireWebSocket(t)
	writer, _ := newUser(t)
	reader, _ := newUser(t)
	start := drainEventCursor(t, reader)
	conn := dialWebSocket(t, reader, webSocketPath(start, nil))

	var thread jmap
	writer.do("POST", "/v1/threads", jmap{"title": randName("wsreconnect"), "content": "first"}, nil).
		expect(t, http.StatusCreated).decode(t, &thread)
	first := readWebSocketEvent(t, conn)
	lastApplied := jstr(first, "seq")
	if lastApplied == "" {
		t.Fatalf("first websocket event has no seq: %+v", first)
	}
	conn.CloseNow() // abrupt client-side disconnect; no close handshake/session resume

	var reply jmap
	writer.do("POST", "/v1/threads/"+jstr(thread, "id")+"/messages", jmap{"content": "while disconnected"}, nil).
		expect(t, http.StatusCreated).decode(t, &reply)
	writer.do("PATCH", "/v1/messages/"+jstr(reply, "id"), jmap{"content": "still disconnected"}, nil).expect(t, http.StatusOK)

	want := pollEventsAfter(t, reader, lastApplied, nil)
	if len(want) == 0 {
		t.Fatal("no events remained for reconnect")
	}
	full := pollEventsAfter(t, reader, start, nil)
	if len(full) != len(want)+1 || !reflect.DeepEqual(full[0], first) {
		t.Fatalf("first applied event does not join cleanly to the resumed poll tail\nfirst: %+v\nfull: %+v", first, full)
	}
	assertEventSequencesEqual(t, full[1:], want)

	reconnected := dialWebSocket(t, reader, webSocketPath(lastApplied, nil))
	got := readWebSocketEvents(t, reconnected, len(want))
	for _, event := range got {
		if jstr(event, "seq") == lastApplied {
			t.Fatalf("reconnect duplicated the last applied seq %s", lastApplied)
		}
	}
	assertEventSequencesEqual(t, got, want)
}

func TestWebSocketFilters(t *testing.T) {
	requireWebSocket(t)
	writer, _ := newUser(t)
	reader, readerName := newUser(t)
	_, outsiderName := newUser(t)
	start := drainEventCursor(t, reader)
	tag := randName("wsfilter")

	var tagged, control, visibleDM, hiddenDM jmap
	writer.do("POST", "/v1/threads", jmap{
		"title": randName("tagged"), "content": "ping @" + readerName, "tags": []string{tag},
	}, nil).expect(t, http.StatusCreated).decode(t, &tagged)
	writer.do("POST", "/v1/threads", jmap{"title": randName("control"), "content": "public control"}, nil).
		expect(t, http.StatusCreated).decode(t, &control)
	writer.do("POST", "/v1/threads", jmap{
		"title": randName("visible-dm"), "content": "reader DM", "participants": []string{readerName},
	}, nil).expect(t, http.StatusCreated).decode(t, &visibleDM)
	writer.do("POST", "/v1/threads", jmap{
		"title": randName("hidden-dm"), "content": "ping @" + readerName, "participants": []string{outsiderName},
	}, nil).expect(t, http.StatusCreated).decode(t, &hiddenDM)
	// Matching sentinels after all excluded traffic ensure an implementation
	// cannot leak an extra frame at the end and escape the finite comparison.
	writer.do("POST", "/v1/threads/"+jstr(tagged, "id")+"/messages", jmap{"content": "after exclusions @" + readerName}, nil).
		expect(t, http.StatusCreated)
	writer.do("POST", "/v1/threads/"+jstr(visibleDM, "id")+"/messages", jmap{"content": "after hidden DM"}, nil).
		expect(t, http.StatusCreated)

	tests := []struct {
		name       string
		filters    url.Values
		wantThread string
	}{
		{name: "mentions", filters: url.Values{"mentions": {"true"}}, wantThread: jstr(tagged, "id")},
		{name: "dms", filters: url.Values{"dms": {"true"}}, wantThread: jstr(visibleDM, "id")},
		{name: "tag", filters: url.Values{"tag": {tag}}, wantThread: jstr(tagged, "id")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := pollEventsAfter(t, reader, start, tc.filters)
			if len(want) == 0 {
				t.Fatalf("%s poll filter returned no events", tc.name)
			}
			for _, event := range want {
				if got := eventThreadID(event); got != tc.wantThread {
					t.Fatalf("%s poll filter leaked thread %s (control %s, hidden DM %s): %+v", tc.name,
						got, jstr(control, "id"), jstr(hiddenDM, "id"), event)
				}
			}
			conn := dialWebSocket(t, reader, webSocketPath(start, tc.filters))
			got := readWebSocketEvents(t, conn, len(want))
			assertEventSequencesEqual(t, got, want)
		})
	}
}

func TestWebSocketHandshakeProblems(t *testing.T) {
	requireWebSocket(t)
	alice, _ := newUser(t)

	unauthorized := failedWebSocketDial(t, &Client{t: t}, "/v1/events/ws", http.StatusUnauthorized)
	assertProblem(t, unauthorized, "unauthorized")
	badCursor := failedWebSocketDial(t, alice, "/v1/events/ws?cursor=banana", http.StatusBadRequest)
	assertProblem(t, badCursor, "validation")
	plainGET := alice.do("GET", "/v1/events/ws", nil, nil).expect(t, http.StatusBadRequest)
	assertProblem(t, plainGET, "validation")
}
