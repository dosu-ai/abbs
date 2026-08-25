package conformance

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestConditionalAnonymousReadSurface(t *testing.T) {
	alice, aliceName := newUser(t)
	bob, bobName := newUser(t)

	publicTag := randName("public")
	dmTag := randName("dm-only")
	var older, newer, dm jmap
	alice.do("POST", "/v1/threads", jmap{
		"title": "older", "content": "v1", "tags": []string{publicTag},
	}, nil).expect(t, http.StatusCreated).decode(t, &older)
	alice.do("POST", "/v1/threads", jmap{
		"title": "newer", "content": "second", "tags": []string{publicTag},
	}, nil).expect(t, http.StatusCreated).decode(t, &newer)
	alice.do("POST", "/v1/threads", jmap{
		"title": "secret", "content": "dm", "tags": []string{dmTag}, "participants": []string{bobName},
	}, nil).expect(t, http.StatusCreated).decode(t, &dm)

	olderID := jstr(older, "id")
	dmID := jstr(dm, "id")
	var messages struct {
		Items []jmap `json:"items"`
	}
	alice.do("GET", "/v1/threads/"+olderID+"/messages", nil, nil).expect(t, http.StatusOK).decode(t, &messages)
	firstMessageID := jstr(messages.Items[0], "id")
	bob.do("POST", "/v1/threads/"+olderID+"/messages", jmap{"content": "reply"}, nil).expect(t, http.StatusCreated)
	bob.do("PUT", "/v1/messages/"+firstMessageID+"/reactions/"+url.PathEscape("👍"), nil, nil).
		expect(t, http.StatusNoContent)
	alice.do("PATCH", "/v1/messages/"+firstMessageID, jmap{"content": "v2"}, nil).expect(t, http.StatusOK)

	anon := &Client{t: t}
	reader := alice
	if visibility == "public" {
		reader = anon
	}
	filters := make([]string, 17)
	for i := range filters {
		filters[i] = "tag=" + url.QueryEscape(publicTag+string(rune('a'+i)))
	}
	reader.do("GET", "/v1/threads?"+strings.Join(filters, "&"), nil, nil).expect(t, http.StatusBadRequest)
	reader.do("GET", "/v1/threads?tag="+url.QueryEscape(strings.Repeat("界", 65)), nil, nil).
		expect(t, http.StatusBadRequest)

	if visibility == "private" {
		for _, path := range []string{
			"/v1/users/" + aliceName,
			"/v1/threads",
			"/v1/threads/" + olderID,
			"/v1/threads/" + olderID + "/messages",
			"/v1/tags",
		} {
			anon.do("GET", path, nil, nil).expect(t, http.StatusUnauthorized)
		}
		return
	}

	// Exact-handle lookup exposes only the deliberately minimal provenance.
	profile := decodeMap(t, anon.do("GET", "/v1/users/"+aliceName, nil, nil).expect(t, http.StatusOK))
	keys := make([]string, 0, len(profile))
	for key := range profile {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"kind", "username"}) {
		t.Fatalf("anonymous profile keys = %v: %+v", keys, profile)
	}
	namedUser := randName("named")
	issuer := &Client{t: t, token: adminToken}
	if authMode == "first-claim" {
		issuer = alice
	}
	issuer.do("POST", "/v1/users", jmap{
		"username": namedUser, "kind": "human", "display_name": "Visible Name",
	}, nil).expect(t, http.StatusCreated)
	namedProfile := decodeMap(t, anon.do("GET", "/v1/users/"+namedUser, nil, nil).expect(t, http.StatusOK))
	namedKeys := make([]string, 0, len(namedProfile))
	for key := range namedProfile {
		namedKeys = append(namedKeys, key)
	}
	sort.Strings(namedKeys)
	if !reflect.DeepEqual(namedKeys, []string{"display_name", "kind", "username"}) ||
		namedProfile["display_name"] != "Visible Name" {
		t.Fatalf("named anonymous profile = %+v", namedProfile)
	}
	anon.do("GET", "/v1/users/does-not-exist", nil, nil).expect(t, http.StatusNotFound)

	// The same isolated public slice produces identical authenticated and
	// anonymous pages, including pagination tokens, ordering, and as_of.
	query := "?tag=" + url.QueryEscape(publicTag) + "&limit=1"
	firstAnon := decodeMap(t, anon.do("GET", "/v1/threads"+query, nil, nil).expect(t, http.StatusOK))
	firstAuth := decodeMap(t, alice.do("GET", "/v1/threads"+query, nil, nil).expect(t, http.StatusOK))
	if !reflect.DeepEqual(firstAnon, firstAuth) {
		t.Fatalf("first thread page differs:\nanon=%+v\nauth=%+v", firstAnon, firstAuth)
	}
	items := firstAnon["items"].([]any)
	if len(items) != 1 || jstr(items[0].(jmap), "id") != olderID {
		t.Fatalf("activity ordering did not put edited thread first: %+v", items)
	}
	next, _ := firstAnon["next_page"].(string)
	if next == "" {
		t.Fatal("public pagination did not return a next_page")
	}
	nextQuery := query + "&page=" + url.QueryEscape(next)
	nextAnon := decodeMap(t, anon.do("GET", "/v1/threads"+nextQuery, nil, nil).expect(t, http.StatusOK))
	nextAuth := decodeMap(t, alice.do("GET", "/v1/threads"+nextQuery, nil, nil).expect(t, http.StatusOK))
	if !reflect.DeepEqual(nextAnon, nextAuth) {
		t.Fatalf("second thread page differs:\nanon=%+v\nauth=%+v", nextAnon, nextAuth)
	}

	anon.do("GET", "/v1/threads/"+olderID, nil, nil).expect(t, http.StatusOK)
	messagePath := "/v1/threads/" + olderID + "/messages?limit=1"
	msgAnon := decodeMap(t, anon.do("GET", messagePath, nil, nil).expect(t, http.StatusOK))
	msgAuth := decodeMap(t, alice.do("GET", messagePath, nil, nil).expect(t, http.StatusOK))
	if !reflect.DeepEqual(msgAnon, msgAuth) {
		t.Fatalf("message page differs:\nanon=%+v\nauth=%+v", msgAnon, msgAuth)
	}
	first := msgAnon["items"].([]any)[0].(jmap)
	if jstr(first, "content") != "v2" || first["edited_at"] == nil || len(first["reactions"].([]any)) != 1 {
		t.Fatalf("anonymous edit/reaction view: %+v", first)
	}
	msgNext, _ := msgAnon["next_page"].(string)
	if msgNext == "" {
		t.Fatal("message pagination did not return next_page")
	}
	secondPath := messagePath + "&page=" + url.QueryEscape(msgNext)
	if a, b := decodeMap(t, anon.do("GET", secondPath, nil, nil).expect(t, http.StatusOK)),
		decodeMap(t, alice.do("GET", secondPath, nil, nil).expect(t, http.StatusOK)); !reflect.DeepEqual(a, b) {
		t.Fatalf("second message page differs:\nanon=%+v\nauth=%+v", a, b)
	}

	// DMs and their tags are absent, with detail reads returning 404.
	anon.do("GET", "/v1/threads/"+dmID, nil, nil).expect(t, http.StatusNotFound)
	anon.do("GET", "/v1/threads/"+dmID+"/messages", nil, nil).expect(t, http.StatusNotFound)
	emptyDM := decodeMap(t, anon.do("GET", "/v1/threads?tag="+url.QueryEscape(dmTag), nil, nil).expect(t, http.StatusOK))
	if len(emptyDM["items"].([]any)) != 0 {
		t.Fatalf("DM leaked through thread listing: %+v", emptyDM)
	}
	publicTags := decodeMap(t, anon.do("GET", "/v1/tags?limit=100", nil, nil).expect(t, http.StatusOK))
	for _, raw := range publicTags["items"].([]any) {
		if jstr(raw.(jmap), "name") == dmTag {
			t.Fatalf("DM-only tag leaked: %+v", raw)
		}
	}

	// Any supplied invalid bearer is a 401, never anonymous fallback.
	invalid := &Client{t: t, token: "not-a-real-token"}
	for _, path := range []string{
		"/v1/users/" + aliceName, "/v1/threads", "/v1/threads/" + olderID,
		"/v1/threads/" + olderID + "/messages", "/v1/tags",
	} {
		invalid.do("GET", path, nil, nil).expect(t, http.StatusUnauthorized)
	}

	// Sensitive reads and writes outside the allowlist remain authenticated.
	for _, path := range []string{
		"/v1/users", "/v1/messages/" + firstMessageID,
		"/v1/messages/" + firstMessageID + "/reactions",
		"/v1/threads/" + olderID + "/read-cursor", "/v1/tag-subscriptions",
		"/v1/inbox", "/v1/events", "/v1/events/ws",
	} {
		anon.do("GET", path, nil, nil).expect(t, http.StatusUnauthorized)
	}
	anon.do("POST", "/v1/threads", jmap{"title": "no", "content": "no"}, nil).expect(t, http.StatusUnauthorized)
	anon.do("POST", "/v1/users/"+aliceName+"/deactivate", nil, nil).expect(t, http.StatusUnauthorized)
	anon.do("PATCH", "/v1/threads/"+olderID, jmap{"tags": []string{"no"}}, nil).expect(t, http.StatusUnauthorized)
	anon.do("POST", "/v1/threads/"+olderID+"/messages", jmap{"content": "no"}, nil).
		expect(t, http.StatusUnauthorized)
	anon.do("PUT", "/v1/threads/"+olderID+"/read-cursor", jmap{"seq": "0"}, nil).
		expect(t, http.StatusUnauthorized)
	anon.do("PATCH", "/v1/messages/"+firstMessageID, jmap{"content": "no"}, nil).expect(t, http.StatusUnauthorized)
	anon.do("DELETE", "/v1/messages/"+firstMessageID, nil, nil).expect(t, http.StatusUnauthorized)
	anon.do("PUT", "/v1/messages/"+firstMessageID+"/reactions/"+url.PathEscape("👍"), nil, nil).
		expect(t, http.StatusUnauthorized)
	anon.do("DELETE", "/v1/messages/"+firstMessageID+"/reactions/"+url.PathEscape("👍"), nil, nil).
		expect(t, http.StatusUnauthorized)
	anon.do("PUT", "/v1/tag-subscriptions/no", nil, nil).expect(t, http.StatusUnauthorized)
	anon.do("DELETE", "/v1/tag-subscriptions/no", nil, nil).expect(t, http.StatusUnauthorized)

	// Tombstones remain visible and identical through the anonymous message
	// collection, while message-by-id itself stays authenticated.
	alice.do("DELETE", "/v1/messages/"+firstMessageID, nil, nil).expect(t, http.StatusOK)
	tombAnon := decodeMap(t, anon.do("GET", messagePath, nil, nil).expect(t, http.StatusOK))
	tombAuth := decodeMap(t, alice.do("GET", messagePath, nil, nil).expect(t, http.StatusOK))
	if !reflect.DeepEqual(tombAnon, tombAuth) || tombAnon["items"].([]any)[0].(jmap)["deleted"] != true {
		t.Fatalf("tombstone mismatch:\nanon=%+v\nauth=%+v", tombAnon, tombAuth)
	}
}

func decodeMap(t *testing.T, res result) jmap {
	t.Helper()
	var out jmap
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatalf("decode %q: %v", res.body, err)
	}
	return out
}
