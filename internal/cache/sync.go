package cache

import (
	"context"
	"time"

	"github.com/dosu-ai/abbs/internal/client"
)

// Syncer owns one workspace's cursor-replay loop:
//
//	loop:
//	  batch = GET /v1/events?cursor=X&timeout=30s
//	  BEGIN; apply events; save new cursor; COMMIT
//
// Append-only, single-direction, server-ordered — no conflict resolution
// because nothing conflicts.
type Syncer struct {
	Cache  *Cache
	Client *client.Client
	// Logf receives non-fatal loop errors (the loop retries with backoff).
	Logf func(format string, args ...any)
}

// Ensure bootstraps the cache when it has no cursor — a fresh or deleted
// cache file rebuilds cleanly from the snapshot endpoints.
func (s *Syncer) Ensure(ctx context.Context) error {
	if _, ok, err := s.Cache.Cursor(); err != nil {
		return err
	} else if ok {
		return nil
	}
	return s.Cache.Bootstrap(ctx, s.Client)
}

// Run tails the event stream until ctx is done. Errors are logged and
// retried with backoff; the loop is dumb and safe by design (an empty
// batch echoes the cursor).
func (s *Syncer) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		cursor, ok, err := s.Cache.Cursor()
		if err == nil && !ok {
			err = s.Cache.Bootstrap(ctx, s.Client)
			if err == nil {
				cursor, _, err = s.Cache.Cursor()
			}
		}
		if err == nil {
			b, perr := s.Client.Events(ctx, client.EventsOptions{Cursor: cursor, TimeoutSeconds: 30, Limit: 100})
			if perr == nil {
				err = s.Cache.Apply(b)
			} else {
				err = perr
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if s.Logf != nil {
				s.Logf("cache sync: %v (retrying in %s)", err, backoff)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}
