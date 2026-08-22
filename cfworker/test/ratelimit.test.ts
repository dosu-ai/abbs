// Token-bucket boundary conditions with injected clocks (the black-box
// suite cannot manipulate time).

import { describe, expect, it } from "vitest";
import { RateLimiter } from "../src/ratelimit";

describe("RateLimiter", () => {
  it("allows the full burst, then denies with Retry-After", () => {
    const l = new RateLimiter(60, 1);
    const t0 = 1_000_000;
    for (let i = 0; i < 60; i++) {
      expect(l.allow("alice", t0).ok).toBe(true);
    }
    const denied = l.allow("alice", t0);
    expect(denied.ok).toBe(false);
    expect(denied.retryAfter).toBe(1); // (1 - 0 tokens) / 1 per sec
  });

  it("refills at the configured rate", () => {
    const l = new RateLimiter(2, 1);
    const t0 = 0;
    expect(l.allow("a", t0).ok).toBe(true);
    expect(l.allow("a", t0).ok).toBe(true);
    expect(l.allow("a", t0).ok).toBe(false);
    // 999ms later: still just under one token.
    expect(l.allow("a", t0 + 999).ok).toBe(false);
    // Another full second later the bucket has ≥ 1 token again.
    expect(l.allow("a", t0 + 2000).ok).toBe(true);
  });

  it("caps refill at the burst", () => {
    const l = new RateLimiter(2, 1);
    expect(l.allow("a", 0).ok).toBe(true);
    // A very long idle period must not accumulate more than burst tokens.
    expect(l.allow("a", 3_600_000).ok).toBe(true);
    expect(l.allow("a", 3_600_000).ok).toBe(true);
    expect(l.allow("a", 3_600_000).ok).toBe(false);
  });

  it("reports whole-second waits for slow refills", () => {
    const l = new RateLimiter(1, 0.1); // one token per 10s
    expect(l.allow("a", 0).ok).toBe(true);
    const denied = l.allow("a", 0);
    expect(denied.ok).toBe(false);
    expect(denied.retryAfter).toBe(10);
  });

  it("tracks users independently", () => {
    const l = new RateLimiter(1, 1);
    expect(l.allow("a", 0).ok).toBe(true);
    expect(l.allow("b", 0).ok).toBe(true);
    expect(l.allow("a", 0).ok).toBe(false);
  });
});
