package server

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dosu-ai/abbs/internal/store"
)

// limiter is an in-process token bucket keyed by the caller-selected identity
// (username for writes, observed address for anonymous reads and claims). No
// Redis until a second server node exists.
type limiter struct {
	mu         sync.Mutex
	burst      float64
	refill     float64 // tokens per second
	maxBuckets int
	buckets    map[string]*list.Element
	lru        *list.List
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

func newLimiter(burst int, refillPerSec float64, maxBuckets int) *limiter {
	return &limiter{
		burst: float64(burst), refill: refillPerSec, maxBuckets: maxBuckets,
		buckets: map[string]*list.Element{}, lru: list.New(),
	}
}

// allow consumes one token; when exhausted it reports the seconds to wait.
func (l *limiter) allow(user string, at time.Time) (ok bool, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	elem, found := l.buckets[user]
	var b *bucket
	if !found {
		if l.lru.Len() >= l.maxBuckets {
			oldest := l.lru.Front()
			delete(l.buckets, oldest.Value.(*bucket).key)
			l.lru.Remove(oldest)
		}
		b = &bucket{key: user, tokens: l.burst, last: at}
		elem = l.lru.PushBack(b)
		l.buckets[user] = elem
	} else {
		b = elem.Value.(*bucket)
		l.lru.MoveToBack(elem)
	}
	b.tokens = min(l.burst, b.tokens+at.Sub(b.last).Seconds()*l.refill)
	b.last = at
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	secs := int(math.Ceil((1 - b.tokens) / l.refill))
	if secs < 1 {
		secs = 1
	}
	return false, secs
}

// recorder captures a handler's response so it can be stored and replayed
// for idempotency-key retries.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder { return &recorder{header: http.Header{}, status: http.StatusOK} }

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *recorder) flush(w http.ResponseWriter) {
	for k, vs := range r.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(r.status)
	w.Write(r.body.Bytes())
}

// write wraps a mutating handler with the write-path behaviors: the anonymous
// claim address limit, per-user rate limit, and Idempotency-Key semantics (per
// principal, per endpoint, ≥24h retention; identical replay returns the
// original response; body mismatch is a 409). endpoint is the route pattern —
// the spec's per-endpoint scope.
func (s *Server) write(endpoint string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Read and restore the body: the principal for the claim endpoint,
		// the request hash, and the handler all need it.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "validation", "cannot read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		principal, activeBearer := s.principalFor(r, body)
		if endpoint == "POST /v1/users" && !activeBearer {
			if ok, retryAfter := s.claimLimiter.allow(s.anonymousClientKey(r), time.Now()); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeProblem(w, http.StatusTooManyRequests, "rate-limited", "anonymous claim rate limit")
				return
			}
		}

		if principal != "" {
			if ok, retryAfter := s.limiter.allow(principal, time.Now()); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeProblem(w, http.StatusTooManyRequests, "rate-limited", "per-user write rate limit")
				return
			}
		}

		key := r.Header.Get("Idempotency-Key")
		if key == "" || principal == "" {
			handler(w, r)
			return
		}
		if len(key) > 128 {
			writeProblem(w, http.StatusBadRequest, "validation", "Idempotency-Key over 128 characters")
			return
		}

		sum := sha256.Sum256(body)
		reqHash := hex.EncodeToString(sum[:])

		// One in-flight execution per key: concurrent retries of the same
		// key serialize here, so the loser replays the winner's response
		// instead of double-executing.
		lockKey := principal + "\x00" + endpoint + "\x00" + key
		mu, _ := s.idemLocks.LoadOrStore(lockKey, &sync.Mutex{})
		mu.(*sync.Mutex).Lock()
		defer mu.(*sync.Mutex).Unlock()

		cutoff := time.Now().Add(-24 * time.Hour)
		rec, err := s.store.IdemGet(principal, endpoint, key, cutoff)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if rec != nil {
			if rec.RequestHash != reqHash {
				writeProblem(w, http.StatusConflict, "idempotency-key-conflict",
					"Idempotency-Key was already used with a different request body")
				return
			}
			w.Header().Set("Content-Type", rec.ContentType)
			w.WriteHeader(rec.Status)
			w.Write(rec.Body)
			return
		}

		rr := newRecorder()
		handler(rr, r)
		rr.flush(w)
		if rr.status < 500 {
			_ = s.store.IdemPut(principal, endpoint, key, store.IdemRecord{
				RequestHash: reqHash,
				Status:      rr.status,
				ContentType: rr.header.Get("Content-Type"),
				Body:        rr.body.Bytes(),
			}, time.Now(), cutoff)
		}
	}
}

// principalFor identifies who a write is charged to and whether the request
// carries a bearer credential for an active user. The principal is the active
// bearer user, or — for the unauthenticated claim endpoint — the username
// being claimed. Empty means unidentifiable (the handler's own auth will
// reject it).
func (s *Server) principalFor(r *http.Request, body []byte) (principal string, activeBearer bool) {
	if token, ok := bearerToken(r); ok {
		if user, err := s.store.UserByTokenHash(hashToken(token)); err == nil && !user.Deactivated {
			return user.Username, true
		}
		return "", false
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/users" {
		var req struct {
			Username string `json:"username"`
		}
		if json.Unmarshal(body, &req) == nil && req.Username != "" {
			return "claim:" + req.Username, false
		}
	}
	return "", false
}
