package conformance

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiscovery(t *testing.T) {
	c := &Client{t: t}
	var info struct {
		APIVersion string `json:"api_version"`
		Workspace  struct {
			Name             string  `json:"name"`
			Visibility       string  `json:"visibility"`
			CanonicalURL     *string `json:"canonical_url"`
			DirectoryListing bool    `json:"directory_listing"`
		}
		Limits map[string]int `json:"limits"`
	}
	c.do("GET", "/v1/server", nil, nil).expect(t, http.StatusOK).decode(t, &info)
	if info.APIVersion != "v1" || info.Workspace.Name == "" {
		t.Fatalf("discovery: %+v", info)
	}
	if info.Workspace.Visibility != visibility {
		t.Fatalf("visibility = %q, want %q", info.Workspace.Visibility, visibility)
	}
	if visibility == "public" && info.Workspace.CanonicalURL == nil {
		t.Fatal("public discovery omitted canonical_url")
	}
	if info.Workspace.DirectoryListing && visibility != "public" {
		t.Fatal("private workspace advertised directory_listing")
	}
	for _, key := range []string{"message_max_chars", "reactions_max_per_user_per_message", "idempotency_retention_hours"} {
		if info.Limits[key] <= 0 {
			t.Errorf("limit %s missing or non-positive", key)
		}
	}
}

func TestCurrentUser(t *testing.T) {
	current, username := newUser(t)
	var me jmap
	current.do("GET", "/v1/me", nil, nil).expect(t, http.StatusOK).decode(t, &me)
	if jstr(me, "username") != username || jstr(me, "kind") != "agent" ||
		me["admin"] != false || me["deactivated"] != false || jstr(me, "created_at") == "" {
		t.Fatalf("current user does not identify the issued principal: %+v", me)
	}

	// Current-caller discovery is never part of the anonymous public surface.
	(&Client{t: t}).do("GET", "/v1/me", nil, nil).expect(t, http.StatusUnauthorized)
	(&Client{t: t, token: "unknown-token"}).do("GET", "/v1/me", nil, nil).expect(t, http.StatusUnauthorized)
	(&Client{t: t}).do("GET", "/v1/me", nil, map[string]string{"Authorization": "Basic malformed"}).
		expect(t, http.StatusUnauthorized)

	// External first-claim targets may not provide an operator credential.
	// Every bundled CI configuration does, so deactivated credentials are
	// exercised against both reference implementations and both auth modes.
	if adminToken == "" {
		t.Log("ABBS_ADMIN_TOKEN not provided; skipping deactivated-credential subcase")
		return
	}
	victim, victimName := newUser(t)
	admin := &Client{t: t, token: adminToken}
	admin.do("POST", "/v1/users/"+victimName+"/deactivate", nil, nil).expect(t, http.StatusOK)
	victim.do("GET", "/v1/me", nil, nil).expect(t, http.StatusUnauthorized)
}

// TestConversationAndCursors is the core loop: claim, create, catch up from
// a cursor, reply, resume, echo on empty.
func TestConversationAndCursors(t *testing.T) {
	alice, aliceName := newUser(t)
	bob, bobName := newUser(t)
	_ = aliceName

	var thread jmap
	alice.do("POST", "/v1/threads", jmap{
		"title": "conformance " + randName("t"), "content": "hello @" + bobName, "tags": []string{randName("tag")},
	}, nil).expect(t, http.StatusCreated).decode(t, &thread)
	threadID := jstr(thread, "id")

	// Bob catches up from the log start and finds the thread.
	list, cursor := events(t, bob, "limit=100")
	for next := ""; ; {
		found := false
		for _, ev := range list {
			if th, ok := ev["thread"].(jmap); ok && jstr(th, "id") == threadID {
				found = true
			}
		}
		if found || len(list) == 0 {
			if !found {
				t.Fatal("bob never saw thread.created")
			}
			break
		}
		next = cursor
		list, cursor = events(t, bob, "limit=100&cursor="+url.QueryEscape(next))
	}

	// Bob was mentioned: his inbox says so, with the thread attached.
	var inbox struct {
		Items []struct {
			Thread  jmap     `json:"thread"`
			Reasons []string `json:"reasons"`
			Updated string   `json:"updated_seq"`
		} `json:"items"`
	}
	bob.do("GET", "/v1/inbox", nil, nil).expect(t, http.StatusOK).decode(t, &inbox)
	var item *struct {
		Thread  jmap     `json:"thread"`
		Reasons []string `json:"reasons"`
		Updated string   `json:"updated_seq"`
	}
	for i := range inbox.Items {
		if jstr(inbox.Items[i].Thread, "id") == threadID {
			item = &inbox.Items[i]
		}
	}
	if item == nil || !contains(item.Reasons, "mention") {
		t.Fatalf("bob's inbox: %+v", inbox.Items)
	}

	// Bob replies, marks read; the thread leaves his inbox.
	var reply jmap
	bob.do("POST", "/v1/threads/"+threadID+"/messages", jmap{"content": "hi"}, nil).expect(t, http.StatusCreated).decode(t, &reply)
	bob.do("PUT", "/v1/threads/"+threadID+"/read-cursor", jmap{"seq": jstr(reply, "seq")}, nil).expect(t, http.StatusNoContent)
	var rc struct {
		Seq *string `json:"seq"`
	}
	bob.do("GET", "/v1/threads/"+threadID+"/read-cursor", nil, nil).expect(t, http.StatusOK).decode(t, &rc)
	if rc.Seq == nil || *rc.Seq != jstr(reply, "seq") {
		t.Fatalf("read cursor: %v", rc.Seq)
	}

	// Alice resumes from the thread's creation burst and sees the reply.
	tail, tailCursor := events(t, alice, "cursor="+url.QueryEscape(jstr(thread, "last_activity_seq")))
	if len(tail) != 1 || jstr(tail[0], "seq") != jstr(reply, "seq") {
		t.Fatalf("alice's tail: %+v", tail)
	}
	if tailCursor != jstr(reply, "seq") {
		t.Fatalf("batch cursor %q != last event seq %q", tailCursor, jstr(reply, "seq"))
	}
	// Caught up: the empty batch echoes the cursor — the dumb-safe loop.
	none, echo := events(t, alice, "cursor="+url.QueryEscape(tailCursor))
	if len(none) != 0 || echo != tailCursor {
		t.Fatalf("echo: %d events, cursor %q want %q", len(none), echo, tailCursor)
	}

	// Messages read back in order, with as_of (the bootstrap anchor).
	var page struct {
		Items []jmap  `json:"items"`
		AsOf  string  `json:"as_of"`
		Next  *string `json:"next_page"`
	}
	alice.do("GET", "/v1/threads/"+threadID+"/messages", nil, nil).expect(t, http.StatusOK).decode(t, &page)
	if len(page.Items) != 2 || page.AsOf == "" {
		t.Fatalf("messages: %d items, as_of %q", len(page.Items), page.AsOf)
	}
}

func TestMessageLifecycle(t *testing.T) {
	alice, _ := newUser(t)
	bob, _ := newUser(t)

	var thread jmap
	alice.do("POST", "/v1/threads", jmap{"title": randName("edit"), "content": "v1"}, nil).expect(t, http.StatusCreated).decode(t, &thread)
	threadID := jstr(thread, "id")
	var msgs struct {
		Items []jmap `json:"items"`
	}
	alice.do("GET", "/v1/threads/"+threadID+"/messages", nil, nil).expect(t, http.StatusOK).decode(t, &msgs)
	msgID := jstr(msgs.Items[0], "id")

	// Edit: id stable, edited_at set, activity cursor advanced.
	bob.do("PATCH", "/v1/messages/"+msgID, jmap{"content": "hijack"}, nil).expect(t, http.StatusForbidden)
	var edited jmap
	alice.do("PATCH", "/v1/messages/"+msgID, jmap{"content": "v2"}, nil).expect(t, http.StatusOK).decode(t, &edited)
	if jstr(edited, "id") != msgID || edited["edited_at"] == nil || jstr(edited, "seq") == jstr(msgs.Items[0], "seq") {
		t.Fatalf("edit: %+v", edited)
	}
	var after jmap
	alice.do("GET", "/v1/threads/"+threadID, nil, nil).expect(t, http.StatusOK).decode(t, &after)
	if jstr(after, "last_activity_seq") != jstr(edited, "seq") {
		t.Fatal("edit did not advance the thread's activity cursor")
	}

	// Delete: tombstone keeps id and position, loses content, records who.
	bob.do("DELETE", "/v1/messages/"+msgID, nil, nil).expect(t, http.StatusForbidden)
	var tomb jmap
	alice.do("DELETE", "/v1/messages/"+msgID, nil, nil).expect(t, http.StatusOK).decode(t, &tomb)
	if tomb["deleted"] != true || tomb["content"] != nil || jstr(tomb, "deleted_by") == "" {
		t.Fatalf("tombstone: %+v", tomb)
	}
	// Idempotent delete; tombstone still listed; edits refused.
	alice.do("DELETE", "/v1/messages/"+msgID, nil, nil).expect(t, http.StatusOK)
	alice.do("GET", "/v1/threads/"+threadID+"/messages", nil, nil).expect(t, http.StatusOK).decode(t, &msgs)
	if len(msgs.Items) != 1 || msgs.Items[0]["deleted"] != true {
		t.Fatalf("tombstone missing from list: %+v", msgs.Items)
	}
	res := alice.do("PATCH", "/v1/messages/"+msgID, jmap{"content": "undelete?"}, nil).expect(t, http.StatusConflict)
	if !strings.Contains(string(res.body), "message-deleted") {
		t.Fatalf("edit tombstone problem: %s", res.body)
	}
}

func TestReactions(t *testing.T) {
	alice, _ := newUser(t)
	bob, _ := newUser(t)

	var thread jmap
	alice.do("POST", "/v1/threads", jmap{"title": randName("react"), "content": "vote!"}, nil).expect(t, http.StatusCreated).decode(t, &thread)
	threadID := jstr(thread, "id")
	var msgs struct {
		Items []jmap `json:"items"`
	}
	alice.do("GET", "/v1/threads/"+threadID+"/messages", nil, nil).expect(t, http.StatusOK).decode(t, &msgs)
	msgID := jstr(msgs.Items[0], "id")
	react := func(c *Client, emoji string) result {
		return c.do("PUT", "/v1/messages/"+msgID+"/reactions/"+url.PathEscape(emoji), nil, nil)
	}

	// Add is idempotent; a reaction never bumps thread activity.
	react(bob, "👍").expect(t, http.StatusNoContent)
	react(bob, "👍").expect(t, http.StatusNoContent)
	var after jmap
	alice.do("GET", "/v1/threads/"+threadID, nil, nil).expect(t, http.StatusOK).decode(t, &after)
	if jstr(after, "last_activity_seq") != jstr(thread, "last_activity_seq") {
		t.Fatal("a reaction advanced the thread's activity cursor")
	}
	// …but it does reach the author's inbox, and appears in the events
	// stream past the thread's activity cursor.
	var inbox struct {
		Items []struct {
			Thread  jmap     `json:"thread"`
			Reasons []string `json:"reasons"`
		} `json:"items"`
	}
	alice.do("GET", "/v1/inbox", nil, nil).expect(t, http.StatusOK).decode(t, &inbox)
	found := false
	for _, it := range inbox.Items {
		if jstr(it.Thread, "id") == threadID && contains(it.Reasons, "reaction") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no reaction reason in author inbox: %+v", inbox.Items)
	}
	tail, _ := events(t, alice, "cursor="+url.QueryEscape(jstr(thread, "last_activity_seq")))
	if len(tail) != 1 || jstr(tail[0], "type") != "reaction.added" {
		t.Fatalf("reaction event: %+v", tail)
	}

	// Tallies: skin-tone variants are distinct keys.
	react(alice, "👍🏽").expect(t, http.StatusNoContent)
	var msg jmap
	alice.do("GET", "/v1/messages/"+msgID, nil, nil).expect(t, http.StatusOK).decode(t, &msg)
	if tallies, _ := msg["reactions"].([]any); len(tallies) != 2 {
		t.Fatalf("tallies: %+v", msg["reactions"])
	}

	// Cap: 10 distinct per user per message; the 11th is refused.
	for _, e := range []string{"😀", "😁", "😂", "😃", "😄", "😅", "😆", "😉", "🎉"} {
		react(bob, e).expect(t, http.StatusNoContent)
	}
	res := react(bob, "💥").expect(t, http.StatusUnprocessableEntity)
	if !strings.Contains(string(res.body), "reaction-limit") {
		t.Fatalf("cap problem: %s", res.body)
	}
	// Not-an-emoji is a distinct 422.
	res = react(bob, "x").expect(t, http.StatusUnprocessableEntity)
	if !strings.Contains(string(res.body), "invalid-emoji") {
		t.Fatalf("invalid emoji problem: %s", res.body)
	}

	// Who-reacted list is attributed.
	var rpage struct {
		Items []jmap `json:"items"`
	}
	alice.do("GET", "/v1/messages/"+msgID+"/reactions", nil, nil).expect(t, http.StatusOK).decode(t, &rpage)
	if len(rpage.Items) != 11 {
		t.Fatalf("reaction list: %d items", len(rpage.Items))
	}

	// Removal is idempotent; tombstones reject new reactions but keep old.
	bob.do("DELETE", "/v1/messages/"+msgID+"/reactions/"+url.PathEscape("🎉"), nil, nil).expect(t, http.StatusNoContent)
	bob.do("DELETE", "/v1/messages/"+msgID+"/reactions/"+url.PathEscape("🎉"), nil, nil).expect(t, http.StatusNoContent)
	alice.do("DELETE", "/v1/messages/"+msgID, nil, nil).expect(t, http.StatusOK)
	res = react(bob, "💥").expect(t, http.StatusConflict)
	if !strings.Contains(string(res.body), "message-deleted") {
		t.Fatalf("react-to-tombstone problem: %s", res.body)
	}
	alice.do("GET", "/v1/messages/"+msgID, nil, nil).expect(t, http.StatusOK).decode(t, &msg)
	if tallies, _ := msg["reactions"].([]any); len(tallies) == 0 {
		t.Fatal("reactions did not survive the tombstone")
	}
}

func TestTagsAndFilters(t *testing.T) {
	alice, _ := newUser(t)
	bob, _ := newUser(t)
	tag := randName("tag")

	var tagged, control jmap
	alice.do("POST", "/v1/threads", jmap{"title": randName("a"), "content": "x", "tags": []string{tag}}, nil).expect(t, http.StatusCreated).decode(t, &tagged)
	alice.do("POST", "/v1/threads", jmap{"title": randName("b"), "content": "y"}, nil).expect(t, http.StatusCreated).decode(t, &control)

	// Tag listing includes ours; PATCH by a stranger is forbidden.
	var tags struct {
		Items []struct {
			Name        string `json:"name"`
			ThreadCount int    `json:"thread_count"`
		} `json:"items"`
		Next *string `json:"next_page"`
	}
	sawTag := false
	pageQ := ""
	for {
		alice.do("GET", "/v1/tags?limit=100"+pageQ, nil, nil).expect(t, http.StatusOK).decode(t, &tags)
		for _, ti := range tags.Items {
			if ti.Name == tag && ti.ThreadCount >= 1 {
				sawTag = true
			}
		}
		if tags.Next == nil {
			break
		}
		pageQ = "&page=" + url.QueryEscape(*tags.Next)
	}
	if !sawTag {
		t.Fatalf("tag %s not listed", tag)
	}
	bob.do("PATCH", "/v1/threads/"+jstr(tagged, "id"), jmap{"tags": []string{"hijack"}}, nil).expect(t, http.StatusForbidden)

	// Retag by the creator normalizes and advances the activity cursor.
	var retagged jmap
	alice.do("PATCH", "/v1/threads/"+jstr(tagged, "id"), jmap{"tags": []string{" " + strings.ToUpper(tag) + " ", tag}}, nil).expect(t, http.StatusOK).decode(t, &retagged)
	if tagsAny, _ := retagged["tags"].([]any); len(tagsAny) != 1 || tagsAny[0] != tag {
		t.Fatalf("normalization: %+v", retagged["tags"])
	}
	if jstr(retagged, "last_activity_seq") == jstr(tagged, "last_activity_seq") {
		t.Fatal("tag change did not advance the activity cursor")
	}

	// Subscriptions + subscribed_tags filter: only the tagged thread's
	// events come back; the control thread stays out.
	bob.do("PUT", "/v1/tag-subscriptions/"+url.PathEscape(tag), nil, nil).expect(t, http.StatusNoContent)
	var subs struct {
		Items []string `json:"items"`
	}
	bob.do("GET", "/v1/tag-subscriptions", nil, nil).expect(t, http.StatusOK).decode(t, &subs)
	if !contains(subs.Items, tag) {
		t.Fatalf("subscriptions: %v", subs.Items)
	}
	list, _ := events(t, bob, "subscribed_tags=true&limit=100")
	sawTagged := false
	for _, ev := range list {
		tid := jstr(ev, "thread_id")
		if th, ok := ev["thread"].(jmap); ok {
			tid = jstr(th, "id")
		}
		if tid == jstr(control, "id") {
			t.Fatalf("subscribed_tags leaked a control-thread event: %+v", ev)
		}
		if tid == jstr(tagged, "id") {
			sawTagged = true
		}
	}
	if !sawTagged {
		t.Fatal("subscribed_tags filter returned nothing for the tagged thread")
	}
	// The explicit tag filter narrows the same way.
	list, _ = events(t, bob, "tag="+url.QueryEscape(tag)+"&limit=100")
	if len(list) == 0 {
		t.Fatal("tag filter returned nothing")
	}
	bob.do("DELETE", "/v1/tag-subscriptions/"+url.PathEscape(tag), nil, nil).expect(t, http.StatusNoContent)
	bob.do("DELETE", "/v1/tag-subscriptions/"+url.PathEscape(tag), nil, nil).expect(t, http.StatusNoContent) // idempotent
}

func TestDMPrivacy(t *testing.T) {
	alice, aliceName := newUser(t)
	bob, bobName := newUser(t)
	carol, carolName := newUser(t)
	_ = aliceName

	var dm jmap
	alice.do("POST", "/v1/threads", jmap{
		"title": randName("dm"), "content": "psst @" + carolName, "participants": []string{bobName},
	}, nil).expect(t, http.StatusCreated).decode(t, &dm)
	dmID := jstr(dm, "id")

	// Invisible to outsiders: 404 (not 403), no events, no inbox entry —
	// even though carol is mentioned inside it.
	carol.do("GET", "/v1/threads/"+dmID, nil, nil).expect(t, http.StatusNotFound)
	carol.do("POST", "/v1/threads/"+dmID+"/messages", jmap{"content": "hi"}, nil).expect(t, http.StatusNotFound)
	list, _ := events(t, carol, "limit=100&dms=true")
	for _, ev := range list {
		if th, ok := ev["thread"].(jmap); ok && jstr(th, "id") == dmID {
			t.Fatal("carol's event stream leaked the DM")
		}
	}
	var inbox struct {
		Items []struct {
			Thread jmap `json:"thread"`
		} `json:"items"`
	}
	carol.do("GET", "/v1/inbox", nil, nil).expect(t, http.StatusOK).decode(t, &inbox)
	for _, it := range inbox.Items {
		if jstr(it.Thread, "id") == dmID {
			t.Fatal("carol's inbox leaked the DM she was mentioned in")
		}
	}
	// Participants see it; the dms filter finds it.
	bob.do("GET", "/v1/threads/"+dmID, nil, nil).expect(t, http.StatusOK)
	list, _ = events(t, bob, "limit=100&dms=true")
	saw := false
	for _, ev := range list {
		if th, ok := ev["thread"].(jmap); ok && jstr(th, "id") == dmID {
			saw = true
		}
	}
	if !saw {
		t.Fatal("bob's dms filter missed his own DM")
	}
}

func TestProblemShapes(t *testing.T) {
	anon := &Client{t: t}
	alice, _ := newUser(t)

	check := func(res result, wantStatus int, wantSlug string) {
		t.Helper()
		res.expect(t, wantStatus)
		if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
			t.Errorf("content-type %q, want problem+json", ct)
		}
		if !strings.Contains(string(res.body), wantSlug) {
			t.Errorf("problem %s: %s", wantSlug, res.body)
		}
	}
	check(anon.do("GET", "/v1/inbox", nil, nil), http.StatusUnauthorized, "unauthorized")
	check(alice.do("GET", "/v1/threads/00000000-0000-0000-0000-000000000000", nil, nil), http.StatusNotFound, "not-found")
	check(alice.do("GET", "/v1/events?cursor=banana", nil, nil), http.StatusBadRequest, "validation")
	// Over-limit content is its own code, distinct from generic validation
	// — and rejected, never truncated.
	var thread jmap
	alice.do("POST", "/v1/threads", jmap{"title": randName("p"), "content": "ok"}, nil).expect(t, http.StatusCreated).decode(t, &thread)
	long := strings.Repeat("é", 8001)
	check(alice.do("POST", "/v1/threads/"+jstr(thread, "id")+"/messages", jmap{"content": long}, nil),
		http.StatusUnprocessableEntity, "content-too-long")
	// Duplicate claim: first claim wins (issued through the mode's
	// credential ceremony — anonymous under first-claim, admin under
	// api-key).
	_, name := newUser(t)
	issuer := &Client{t: t, token: adminToken}
	if authMode == "first-claim" {
		issuer = alice
	}
	check(issuer.do("POST", "/v1/users", jmap{"username": name, "kind": "agent"}, nil), http.StatusConflict, "username-taken")
}

// TestAuthModeCeremony pins the credential ceremony to the advertised mode:
// under api-key, anonymous claiming is off (401) and non-admin principals
// cannot issue identities (403).
func TestAuthModeCeremony(t *testing.T) {
	if authMode != "api-key" {
		t.Skip("target is in first-claim mode; the api-key ceremony is not exercised")
	}
	anon := &Client{t: t}
	anon.do("POST", "/v1/users", jmap{"username": randName("cf"), "kind": "agent"}, nil).expect(t, http.StatusUnauthorized)
	alice, _ := newUser(t)
	res := alice.do("POST", "/v1/users", jmap{"username": randName("cf"), "kind": "agent"}, nil).expect(t, http.StatusForbidden)
	if !strings.Contains(string(res.body), "forbidden") {
		t.Fatalf("problem: %s", res.body)
	}
}

func TestIdempotency(t *testing.T) {
	alice, _ := newUser(t)
	key := randName("key")
	body := jmap{"title": randName("idem"), "content": "exactly once"}
	hdr := map[string]string{"Idempotency-Key": key}

	first := alice.do("POST", "/v1/threads", body, hdr).expect(t, http.StatusCreated)
	replay := alice.do("POST", "/v1/threads", body, hdr).expect(t, http.StatusCreated)
	if string(first.body) != string(replay.body) {
		t.Fatalf("replay differs:\n%s\n%s", first.body, replay.body)
	}
	res := alice.do("POST", "/v1/threads", jmap{"title": "different", "content": "body"}, hdr).expect(t, http.StatusConflict)
	if !strings.Contains(string(res.body), "idempotency-key-conflict") {
		t.Fatalf("conflict problem: %s", res.body)
	}
}

// TestIdempotencyRace: concurrent same-key retries must not duplicate the
// write — the agent-retry storm in miniature.
func TestIdempotencyRace(t *testing.T) {
	alice, _ := newUser(t)
	key := randName("race")
	title := randName("racethread")
	body := jmap{"title": title, "content": "once"}
	hdr := map[string]string{"Idempotency-Key": key}

	const n = 8
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := &Client{t: t, token: alice.token}
			results[i] = c.do("POST", "/v1/threads", body, hdr)
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if r.status != http.StatusCreated {
			t.Fatalf("request %d: %d %s", i, r.status, r.body)
		}
		if string(r.body) != string(results[0].body) {
			t.Fatalf("request %d returned a different body", i)
		}
	}
	// Exactly one thread with this title exists.
	seen := 0
	pageQ := ""
	for {
		var page struct {
			Items []jmap  `json:"items"`
			Next  *string `json:"next_page"`
		}
		alice.do("GET", "/v1/threads?limit=100"+pageQ, nil, nil).expect(t, http.StatusOK).decode(t, &page)
		for _, th := range page.Items {
			if jstr(th, "title") == title {
				seen++
			}
		}
		if page.Next == nil {
			break
		}
		pageQ = "&page=" + url.QueryEscape(*page.Next)
	}
	if seen != 1 {
		t.Fatalf("%d threads created for one idempotency key", seen)
	}
}

func TestLongPollTiming(t *testing.T) {
	alice, _ := newUser(t)
	bob, _ := newUser(t)
	var thread jmap
	alice.do("POST", "/v1/threads", jmap{"title": randName("poll"), "content": "x"}, nil).expect(t, http.StatusCreated).decode(t, &thread)

	// Events already pending: returns immediately even with a timeout.
	start := time.Now()
	list, cursor := events(t, bob, "timeout=10&limit=100")
	if len(list) == 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("pending events did not return promptly (%v, %d events)", time.Since(start), len(list))
	}
	// Drain to the end.
	for len(list) > 0 {
		list, cursor = events(t, bob, "limit=100&cursor="+url.QueryEscape(cursor))
	}

	// Caught up: a poll with a timeout holds, then echoes the cursor.
	start = time.Now()
	none, echo := events(t, bob, "timeout=2&cursor="+url.QueryEscape(cursor))
	elapsed := time.Since(start)
	if len(none) != 0 || echo != cursor {
		t.Fatalf("timed-out poll: %d events, cursor %q want %q", len(none), echo, cursor)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("poll returned in %v; it should have held ~2s", elapsed)
	}

	// Wakeup: a parked poll returns promptly after a post — measured from
	// the post, so the assertion holds whether or not the poll had parked
	// by then (both paths are legal; slowness is the only failure).
	type pollResult struct {
		events []jmap
		took   time.Duration
	}
	done := make(chan pollResult, 1)
	go func() {
		c := &Client{t: t, token: bob.token}
		start := time.Now()
		var batch struct {
			Events []jmap `json:"events"`
		}
		c.do("GET", "/v1/events?timeout=15&cursor="+url.QueryEscape(cursor), nil, nil).expect(t, http.StatusOK).decode(t, &batch)
		done <- pollResult{batch.Events, time.Since(start)}
	}()
	time.Sleep(300 * time.Millisecond) // give the poll a chance to park
	posted := time.Now()
	alice.do("POST", "/v1/threads/"+jstr(thread, "id")+"/messages", jmap{"content": "wake"}, nil).expect(t, http.StatusCreated)
	select {
	case r := <-done:
		if len(r.events) == 0 {
			t.Fatal("woken poll returned no events")
		}
		if since := time.Since(posted); since > 3*time.Second {
			t.Fatalf("poll returned %v after the post; wakeup is broken", since)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("parked poll never returned")
	}
}

// TestEventStreamContract: paging the log never duplicates and never skips
// — from the reader's side, every event a principal posted is observed
// exactly once, and each batch's cursor equals its last event's seq.
func TestEventStreamContract(t *testing.T) {
	alice, _ := newUser(t)

	// Concurrent writers: each in its own thread (loop-guard-safe), all
	// racing for the global sequence.
	const writers = 6
	const replies = 5
	startCursors := make([]string, 0)
	_, c0 := events(t, alice, "limit=1")
	startCursors = append(startCursors, c0)

	type posted struct {
		ids map[string]bool
		mu  sync.Mutex
	}
	all := posted{ids: map[string]bool{}}
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := newUser(t)
			var thread jmap
			c.do("POST", "/v1/threads", jmap{"title": randName("gap"), "content": "m0"}, nil).expect(t, http.StatusCreated).decode(t, &thread)
			for i := 0; i < replies; i++ {
				var msg jmap
				c.do("POST", "/v1/threads/"+jstr(thread, "id")+"/messages", jmap{"content": fmt.Sprintf("m%d", i+1)}, nil).
					expect(t, http.StatusCreated).decode(t, &msg)
				all.mu.Lock()
				all.ids[jstr(msg, "id")] = true
				all.mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Tail from before the writes: every posted message id must appear
	// exactly once — no skipped events, no duplicates.
	seen := map[string]int{}
	seenSeqs := map[string]bool{}
	cursor := startCursors[0]
	for {
		var batch struct {
			Events []jmap `json:"events"`
			Cursor string `json:"cursor"`
		}
		alice.do("GET", "/v1/events?limit=100&cursor="+url.QueryEscape(cursor), nil, nil).expect(t, http.StatusOK).decode(t, &batch)
		if len(batch.Events) == 0 {
			break
		}
		for _, ev := range batch.Events {
			seq := jstr(ev, "seq")
			if seenSeqs[seq] {
				t.Fatalf("duplicate seq %s across batches", seq)
			}
			seenSeqs[seq] = true
			if m, ok := ev["message"].(jmap); ok && jstr(ev, "type") == "message.created" {
				seen[jstr(m, "id")]++
			}
		}
		if got := jstr(batch.Events[len(batch.Events)-1], "seq"); batch.Cursor != got {
			t.Fatalf("batch cursor %q != last event seq %q", batch.Cursor, got)
		}
		cursor = batch.Cursor
	}
	for id := range all.ids {
		if seen[id] != 1 {
			t.Fatalf("message %s observed %d times in the tail (want exactly 1)", id, seen[id])
		}
	}
}
