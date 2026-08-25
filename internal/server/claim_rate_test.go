package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/store"
)

func newDirectHandler(t *testing.T) http.Handler {
	h, _ := newDirectHandlerWithStore(t)
	return h
}

func newDirectHandlerWithStore(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return MustNew(st, Config{WorkspaceName: "claim-rate-test"}), st
}

func directRequest(t *testing.T, h http.Handler, method, path, remoteAddr string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "http://abbs.test"+path, &encoded)
	req.RemoteAddr = remoteAddr
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func directClaim(t *testing.T, h http.Handler, remoteAddr, username string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return directRequest(t, h, http.MethodPost, "/v1/users", remoteAddr,
		api.ClaimUserRequest{Username: username, Kind: "agent"}, headers)
}

func TestAnonymousClaimRateLimitByAddress(t *testing.T) {
	h := newDirectHandler(t)
	for _, username := range []string{"address-a", "address-b", "address-c"} {
		if rr := directClaim(t, h, "203.0.113.8:1234", username, nil); rr.Code != http.StatusCreated {
			t.Fatalf("claim %q = %d: %s", username, rr.Code, rr.Body.String())
		}
	}

	limited := directClaim(t, h, "203.0.113.8:1234", "address-d", nil)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("fourth claim = %d retry=%q: %s", limited.Code, limited.Header().Get("Retry-After"), limited.Body.String())
	}
	if rr := directClaim(t, h, "203.0.113.9:1234", "address-e", nil); rr.Code != http.StatusCreated {
		t.Fatalf("different-address claim = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAnonymousClaimRateLimitFallbackBucket(t *testing.T) {
	h := newDirectHandler(t)
	for _, username := range []string{"fallback-a", "fallback-b", "fallback-c"} {
		if rr := directClaim(t, h, "", username, nil); rr.Code != http.StatusCreated {
			t.Fatalf("claim %q = %d: %s", username, rr.Code, rr.Body.String())
		}
	}

	limited := directClaim(t, h, "", "fallback-d", nil)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("fallback fourth claim = %d retry=%q: %s", limited.Code, limited.Header().Get("Retry-After"), limited.Body.String())
	}
}

func TestInvalidBearerDoesNotBypassAnonymousClaimRateLimit(t *testing.T) {
	h := newDirectHandler(t)
	headers := map[string]string{"Authorization": "Bearer definitely-invalid"}
	for _, username := range []string{"invalid-bearer-a", "invalid-bearer-b", "invalid-bearer-c"} {
		if rr := directClaim(t, h, "203.0.113.18:1234", username, headers); rr.Code != http.StatusCreated {
			t.Fatalf("claim %q = %d: %s", username, rr.Code, rr.Body.String())
		}
	}

	limited := directClaim(t, h, "203.0.113.18:1234", "invalid-bearer-d", headers)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("fourth invalid-bearer claim = %d retry=%q: %s", limited.Code, limited.Header().Get("Retry-After"), limited.Body.String())
	}
}

func TestDeactivatedBearerDoesNotBypassAnonymousClaimRateLimit(t *testing.T) {
	h, st := newDirectHandlerWithStore(t)
	claimed := directClaim(t, h, "203.0.113.28:1234", "deactivated-user", nil)
	if claimed.Code != http.StatusCreated {
		t.Fatalf("initial claim = %d: %s", claimed.Code, claimed.Body.String())
	}
	var credential api.ClaimUserResponse
	if err := json.Unmarshal(claimed.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeactivateUser("deactivated-user", time.Now()); err != nil {
		t.Fatal(err)
	}

	headers := map[string]string{"Authorization": "Bearer " + credential.Token}
	for _, username := range []string{"deactivated-bearer-a", "deactivated-bearer-b", "deactivated-bearer-c"} {
		if rr := directClaim(t, h, "203.0.113.29:1234", username, headers); rr.Code != http.StatusCreated {
			t.Fatalf("claim %q = %d: %s", username, rr.Code, rr.Body.String())
		}
	}
	limited := directClaim(t, h, "203.0.113.29:1234", "deactivated-bearer-d", headers)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth deactivated-bearer claim = %d: %s", limited.Code, limited.Body.String())
	}
}

func TestAnonymousClaimRateLimitLeavesAuthenticatedWritesAndReplayAlone(t *testing.T) {
	h := newDirectHandler(t)
	first := directClaim(t, h, "198.51.100.4:1234", "idem-claim", map[string]string{"Idempotency-Key": "claim-key"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first claim = %d: %s", first.Code, first.Body.String())
	}
	var claimed api.ClaimUserResponse
	if err := json.Unmarshal(first.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"idem-fill-a", "idem-fill-b"} {
		if rr := directClaim(t, h, "198.51.100.4:1234", username, nil); rr.Code != http.StatusCreated {
			t.Fatalf("claim %q = %d: %s", username, rr.Code, rr.Body.String())
		}
	}

	auth := map[string]string{"Authorization": "Bearer " + claimed.Token}
	thread := directRequest(t, h, http.MethodPost, "/v1/threads", "198.51.100.4:1234",
		api.CreateThreadRequest{Title: "authenticated", Content: "still allowed"}, auth)
	if thread.Code != http.StatusCreated {
		t.Fatalf("authenticated write = %d: %s", thread.Code, thread.Body.String())
	}
	if rr := directClaim(t, h, "198.51.100.4:1234", "authenticated-claim", auth); rr.Code != http.StatusCreated {
		t.Fatalf("authenticated claim = %d: %s", rr.Code, rr.Body.String())
	}

	replay := directClaim(t, h, "198.51.100.5:1234", "idem-claim", map[string]string{"Idempotency-Key": "claim-key"})
	if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
		t.Fatalf("cross-address replay = %d equal=%v: %s", replay.Code, replay.Body.String() == first.Body.String(), replay.Body.String())
	}
}
