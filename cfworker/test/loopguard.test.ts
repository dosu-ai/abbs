// Reply-loop guard boundaries with injected clocks.

import { describe, expect, it } from "vitest";
import { DEFAULT_LOOP_GUARD, loopGuardTrips, retryAfterSeconds } from "../src/loopguard";

const cfg = DEFAULT_LOOP_GUARD; // 10 messages, 2-minute window

function pingPong(n: number): string[] {
  return Array.from({ length: n }, (_, i) => (i % 2 === 0 ? "a" : "b"));
}

describe("loopGuardTrips", () => {
  const now = 1_000_000_000;

  it("trips on a rapid two-author ping-pong", () => {
    expect(loopGuardTrips(pingPong(10), now - 60_000, "a", now, cfg)).toBe(true);
  });

  it("does not trip below the message threshold", () => {
    expect(loopGuardTrips(pingPong(9), now - 1_000, "a", now, cfg)).toBe(false);
  });

  it("does not trip when a third author joins", () => {
    expect(loopGuardTrips(pingPong(10), now - 60_000, "c", now, cfg)).toBe(false);
  });

  it("does not trip once the window has elapsed (boundary is exclusive)", () => {
    // Go: time.Since(oldest) < window — exactly the window is NOT inside it.
    expect(loopGuardTrips(pingPong(10), now - cfg.windowMs, "a", now, cfg)).toBe(false);
    expect(loopGuardTrips(pingPong(10), now - cfg.windowMs + 1, "a", now, cfg)).toBe(true);
  });

  it("trips for a single author flooding alone", () => {
    expect(loopGuardTrips(Array(10).fill("a"), now - 1_000, "a", now, cfg)).toBe(true);
  });

  it("Retry-After is half the window, at least 1s", () => {
    expect(retryAfterSeconds(cfg)).toBe(60);
    expect(retryAfterSeconds({ messages: 10, windowMs: 1000 })).toBe(1);
  });
});
