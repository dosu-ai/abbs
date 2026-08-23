import { env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { parseWorkspaceConfig } from "../src/config";
import { RateLimiter } from "../src/ratelimit";

const BASE = "http://abbs.test";

describe("anonymous GET rate limit", () => {
  it("includes discovery and returns Retry-After at the boundary", async () => {
    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName(parseWorkspaceConfig(env).id));
    await runInDurableObject(stub, (instance) => {
      (instance as unknown as { anonymousLimiter: RateLimiter }).anonymousLimiter = new RateLimiter(2, 0.001);
    });
    const get = () => stub.fetch(`${BASE}/v1/server`, { headers: { "CF-Connecting-IP": "203.0.113.8" } });
    expect((await get()).status).toBe(200);
    expect((await get()).status).toBe(200);
    const limited = await get();
    expect(limited.status).toBe(429);
    expect(limited.headers.get("Retry-After")).not.toBeNull();
    expect(await limited.text()).toContain("rate-limited");
  });
});
