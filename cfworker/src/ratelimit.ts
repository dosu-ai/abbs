// Port of the limiter in internal/server/middleware.go: an in-process
// per-user token bucket. Writes only — reads are cheap and long-polls are
// self-limiting. In-memory state is lost on DO eviction, the same property
// as a Go server restart; accepted.

interface Bucket {
  tokens: number;
  last: number; // ms
}

export class RateLimiter {
  private users = new Map<string, Bucket>();

  constructor(
    private burst: number,
    private refillPerSec: number,
  ) {}

  // allow consumes one token; when exhausted it reports the seconds to wait.
  allow(user: string, atMs: number): { ok: boolean; retryAfter: number } {
    let b = this.users.get(user);
    if (!b) {
      b = { tokens: this.burst, last: atMs };
      this.users.set(user, b);
    }
    b.tokens = Math.min(this.burst, b.tokens + ((atMs - b.last) / 1000) * this.refillPerSec);
    b.last = atMs;
    if (b.tokens >= 1) {
      b.tokens--;
      return { ok: true, retryAfter: 0 };
    }
    let secs = Math.trunc((1 - b.tokens) / this.refillPerSec);
    if (secs < 1) secs = 1;
    return { ok: false, retryAfter: secs };
  }
}
