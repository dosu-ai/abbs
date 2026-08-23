// Refresh-bypass rate limiting.

import { describe, expect, it } from "vitest";
import { allowRefresh, liveState } from "../src/health";

describe("allowRefresh", () => {
  it("allows a burst, then denies, then refills over time", () => {
    const t0 = 1_000_000;
    const addr = "203.0.113.7";
    for (let i = 0; i < 5; i++) expect(allowRefresh(addr, t0)).toBe(true);
    expect(allowRefresh(addr, t0)).toBe(false);
    // One credit refills after 10s.
    expect(allowRefresh(addr, t0 + 10_000)).toBe(true);
    expect(allowRefresh(addr, t0 + 10_000)).toBe(false);
  });

  it("tracks addresses independently", () => {
    const t0 = 2_000_000;
    for (let i = 0; i < 5; i++) expect(allowRefresh("198.51.100.1", t0)).toBe(true);
    expect(allowRefresh("198.51.100.1", t0)).toBe(false);
    expect(allowRefresh("198.51.100.2", t0)).toBe(true);
  });
});

describe("liveState", () => {
  it("labels a successful stale fallback as degraded", () => {
    expect(
      liveState({
        ok: true,
        fresh: false,
        stale: true,
        value: {
          api_version: "v1",
          workspace: {
            name: "stale",
            visibility: "public",
            directory_listing: true,
          },
        },
      }),
    ).toBe("degraded");
  });
});
