import { env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { WorkspaceDO } from "../src/workspace-do";

const BASE = "http://abbs.test";

type Fetcher = {
  fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response>;
};

function workspace(name: string): Fetcher {
  return env.WORKSPACE.get(env.WORKSPACE.idFromName(name));
}

function claim(
  stub: Fetcher,
  username: string,
  ip?: string,
  options: { token?: string; idempotencyKey?: string } = {},
): Promise<Response> {
  return stub.fetch(`${BASE}/v1/users`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(ip !== undefined ? { "CF-Connecting-IP": ip } : {}),
      ...(options.token !== undefined ? { Authorization: `Bearer ${options.token}` } : {}),
      ...(options.idempotencyKey !== undefined ? { "Idempotency-Key": options.idempotencyKey } : {}),
    },
    body: JSON.stringify({ username, kind: "agent" }),
  });
}

describe("anonymous claim rate limit", () => {
  it("limits distinct names per address while leaving another address available", async () => {
    const stub = workspace("claim-rate-addresses");
    for (const name of ["address-a", "address-b", "address-c"]) {
      expect((await claim(stub, name, "203.0.113.8")).status).toBe(201);
    }

    const limited = await claim(stub, "address-d", "203.0.113.8");
    expect(limited.status).toBe(429);
    expect(limited.headers.get("Retry-After")).not.toBeNull();
    expect(await limited.text()).toContain("rate-limited");

    expect((await claim(stub, "address-e", "203.0.113.9")).status).toBe(201);
  });

  it("uses one shared fallback bucket when CF-Connecting-IP is missing", async () => {
    const stub = workspace("claim-rate-fallback");
    for (const name of ["fallback-a", "fallback-b", "fallback-c"]) {
      expect((await claim(stub, name)).status).toBe(201);
    }

    const limited = await claim(stub, "fallback-d");
    expect(limited.status).toBe(429);
    expect(limited.headers.get("Retry-After")).not.toBeNull();
  });

  it("does not let an invalid bearer bypass the address limit", async () => {
    const stub = workspace("claim-rate-invalid-bearer");
    for (const name of ["invalid-bearer-a", "invalid-bearer-b", "invalid-bearer-c"]) {
      expect((await claim(stub, name, "203.0.113.18", { token: "definitely-invalid" })).status).toBe(201);
    }

    const limited = await claim(stub, "invalid-bearer-d", "203.0.113.18", { token: "definitely-invalid" });
    expect(limited.status).toBe(429);
    expect(limited.headers.get("Retry-After")).not.toBeNull();
  });

  it("does not let a deactivated bearer bypass the address limit", async () => {
    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName("claim-rate-deactivated-bearer"));
    const initial = await claim(stub, "deactivated-user", "203.0.113.28");
    expect(initial.status).toBe(201);
    const token = ((await initial.json()) as { token: string }).token;
    await runInDurableObject(stub, (instance) => {
      (instance as unknown as WorkspaceDO).store.sql.exec(
        `UPDATE users SET deactivated = 1 WHERE username = 'deactivated-user'`,
      );
    });

    for (const name of ["deactivated-bearer-a", "deactivated-bearer-b", "deactivated-bearer-c"]) {
      expect((await claim(stub, name, "203.0.113.29", { token })).status).toBe(201);
    }
    expect((await claim(stub, "deactivated-bearer-d", "203.0.113.29", { token })).status).toBe(429);
  });

  it("groups IPv6 clients by their /64 prefix", async () => {
    const stub = workspace("claim-rate-ipv6");
    const samePrefix = ["2001:db8:abcd:12::1", "2001:db8:abcd:12::2", "2001:db8:abcd:12:ffff::1"];
    for (const [i, ip] of samePrefix.entries()) {
      expect((await claim(stub, `ipv6-${i}`, ip)).status).toBe(201);
    }

    expect((await claim(stub, "ipv6-limited", "2001:db8:abcd:12::ffff")).status).toBe(429);
    expect((await claim(stub, "ipv6-other", "2001:db8:abcd:13::1")).status).toBe(201);
  });

  it("does not affect authenticated writes or cross-address idempotent claim replay", async () => {
    const stub = workspace("claim-rate-auth-idem");
    const body = JSON.stringify({ username: "idem-claim", kind: "agent" });
    const first = await claim(stub, "idem-claim", "198.51.100.4", { idempotencyKey: "claim-key" });
    expect(first.status).toBe(201);
    const firstText = await first.text();
    const token = (JSON.parse(firstText) as { token: string }).token;

    expect((await claim(stub, "idem-fill-a", "198.51.100.4")).status).toBe(201);
    expect((await claim(stub, "idem-fill-b", "198.51.100.4")).status).toBe(201);

    const thread = await stub.fetch(`${BASE}/v1/threads`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
        "CF-Connecting-IP": "198.51.100.4",
      },
      body: JSON.stringify({ title: "authenticated", content: "still allowed" }),
    });
    expect(thread.status).toBe(201);

    expect((await claim(stub, "authenticated-claim", "198.51.100.4", { token })).status).toBe(201);

    const replay = await stub.fetch(`${BASE}/v1/users`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "CF-Connecting-IP": "198.51.100.5",
        "Idempotency-Key": "claim-key",
      },
      body,
    });
    expect(replay.status).toBe(201);
    expect(await replay.text()).toBe(firstText);
  });
});
