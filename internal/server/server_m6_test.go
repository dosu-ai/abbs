package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/store"
)

// newAPIKeyServer boots a server in api-key mode with one operator-created
// admin (the abbs admin create-user ceremony, done against the store).
func newAPIKeyServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "abbs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	token, tokenHash := NewToken()
	if _, err := st.ClaimUser("op", "human", nil, tokenHash, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAdmin("op", true); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, Config{AuthMode: AuthAPIKey}))
	t.Cleanup(srv.Close)
	return srv, token
}

func TestAPIKeyMode(t *testing.T) {
	srv, adminToken := newAPIKeyServer(t)

	// Discovery advertises the selected mode.
	anon := &client{t: t, base: srv.URL}
	var info api.ServerInfo
	anon.do("GET", "/v1/server", nil, http.StatusOK, &info)
	if len(info.AuthModes) != 1 || info.AuthModes[0] != AuthAPIKey {
		t.Fatalf("auth_modes = %v, want [api-key]", info.AuthModes)
	}

	// Anonymous claiming is off.
	anon.do("POST", "/v1/users", api.ClaimUserRequest{Username: "sneaky", Kind: "agent"}, http.StatusUnauthorized, nil)

	// An admin issues a key; the key authenticates normally.
	admin := &client{t: t, base: srv.URL, token: adminToken}
	var issued api.ClaimUserResponse
	admin.do("POST", "/v1/users", api.ClaimUserRequest{Username: "bot", Kind: "agent"}, http.StatusCreated, &issued)
	if issued.Token == "" || issued.User.Username != "bot" {
		t.Fatalf("issued: %+v", issued)
	}
	bot := &client{t: t, base: srv.URL, token: issued.Token}
	bot.do("GET", "/v1/inbox", nil, http.StatusOK, nil)

	// Non-admins cannot issue identities.
	bot.do("POST", "/v1/users", api.ClaimUserRequest{Username: "bot2", Kind: "agent"}, http.StatusForbidden, nil)
}
