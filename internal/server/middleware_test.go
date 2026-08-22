package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterEvictsLeastRecentlyUsedBucket(t *testing.T) {
	l := newLimiter(1, 1, 2)
	now := time.Unix(1, 0)
	if ok, _ := l.allow("a", now); !ok {
		t.Fatal("first a request denied")
	}
	if ok, _ := l.allow("b", now); !ok {
		t.Fatal("first b request denied")
	}
	if ok, _ := l.allow("a", now); ok {
		t.Fatal("second a request unexpectedly allowed")
	}
	if ok, _ := l.allow("c", now); !ok {
		t.Fatal("first c request denied")
	}
	// Touching a made b the least-recently-used bucket, so admitting c evicted
	// b. Its next request starts with a fresh burst instead of growing the map.
	if ok, _ := l.allow("b", now); !ok {
		t.Fatal("evicted b bucket was not recreated")
	}
}

func TestAnonymousClientKeyTrustedProxyChain(t *testing.T) {
	trusted, err := parseTrustedProxyCIDRs([]string{"127.0.0.0/8", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trustedProxies: trusted}

	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{
			name: "untrusted peer ignores header", remoteAddr: "203.0.113.9:1234",
			forwarded: "198.51.100.4", want: "203.0.113.9",
		},
		{
			name: "trusted multi-hop chain", remoteAddr: "127.0.0.1:1234",
			forwarded: "198.51.100.4, 10.0.0.2", want: "198.51.100.4",
		},
		{
			name: "spoofed left entry is ignored", remoteAddr: "127.0.0.1:1234",
			forwarded: "192.0.2.8, 198.51.100.4", want: "198.51.100.4",
		},
		{
			name: "invalid trusted chain fails closed", remoteAddr: "127.0.0.1:1234",
			forwarded: "not-an-ip", want: "127.0.0.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://example.test/v1/server", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			if got := s.anonymousClientKey(r); got != tt.want {
				t.Fatalf("anonymousClientKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
