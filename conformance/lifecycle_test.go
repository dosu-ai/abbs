package conformance

import (
	"net/http"
	"net/url"
	"testing"
)

// TestKill9Durability is the M2 exit criterion as a standing test: an
// acknowledged write survives SIGKILL, and a pre-kill cursor resumes
// cleanly after restart. Runs only when the suite owns the server process.
func TestKill9Durability(t *testing.T) {
	proc := ownedServer(t)

	alice, _ := provision(t, proc.url, proc.adminToken)
	bob, _ := provision(t, proc.url, proc.adminToken)

	var thread jmap
	alice.do("POST", "/v1/threads", jmap{"title": "durable", "content": "before"}, nil).expect(t, http.StatusCreated).decode(t, &thread)
	threadID := jstr(thread, "id")

	// Bob catches up and remembers his cursor.
	var batch struct {
		Cursor string `json:"cursor"`
	}
	bob.do("GET", "/v1/events?limit=100", nil, nil).expect(t, http.StatusOK).decode(t, &batch)
	preKill := batch.Cursor

	// This write is acknowledged (201) — it must survive anything.
	var acked jmap
	alice.do("POST", "/v1/threads/"+threadID+"/messages", jmap{"content": "acked just before the crash"}, nil).
		expect(t, http.StatusCreated).decode(t, &acked)

	proc.kill9()
	if err := proc.restart(); err != nil {
		t.Fatalf("restart after kill -9: %v", err)
	}

	// Bob resumes from his pre-kill cursor: exactly the acked message.
	var tail struct {
		Events []jmap `json:"events"`
	}
	bob.do("GET", "/v1/events?limit=100&cursor="+url.QueryEscape(preKill), nil, nil).expect(t, http.StatusOK).decode(t, &tail)
	if len(tail.Events) != 1 {
		t.Fatalf("resumed tail has %d events, want 1: %+v", len(tail.Events), tail.Events)
	}
	if m, _ := tail.Events[0]["message"].(jmap); jstr(m, "id") != jstr(acked, "id") {
		t.Fatalf("resumed tail: %+v", tail.Events[0])
	}

	// Life goes on: new writes get strictly later positions and the full
	// thread is intact.
	var after jmap
	alice.do("POST", "/v1/threads/"+threadID+"/messages", jmap{"content": "after restart"}, nil).
		expect(t, http.StatusCreated).decode(t, &after)
	var msgs struct {
		Items []jmap `json:"items"`
	}
	alice.do("GET", "/v1/threads/"+threadID+"/messages", nil, nil).expect(t, http.StatusOK).decode(t, &msgs)
	if len(msgs.Items) != 3 {
		t.Fatalf("thread has %d messages after restart, want 3 (nothing lost)", len(msgs.Items))
	}
}
