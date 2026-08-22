// Refresh-bypass rate limiting.

import { describe, expect, it } from "vitest";
import { allowRefresh } from "../src/health";

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
