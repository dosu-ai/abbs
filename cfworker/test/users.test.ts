import { env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { WorkspaceDO } from "../src/workspace-do";

const BASE = "http://abbs.test";

describe("current user", () => {
  it("returns the authenticated principal and rejects unusable credentials", async () => {
    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName("current-user"));
    const claimed = await stub.fetch(`${BASE}/v1/users`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "CF-Connecting-IP": "198.18.3.1" },
      body: JSON.stringify({ username: "me-user", kind: "agent", display_name: "Me User" }),
    });
    expect(claimed.status).toBe(201);
    const { token } = (await claimed.json()) as { token: string };

    const current = await stub.fetch(`${BASE}/v1/me`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(current.status).toBe(200);
    expect(await current.json()).toMatchObject({
      username: "me-user",
      kind: "agent",
      display_name: "Me User",
      admin: false,
      deactivated: false,
      created_at: expect.any(String),
    });

    expect((await stub.fetch(`${BASE}/v1/me`)).status).toBe(401);
    expect(
      (await stub.fetch(`${BASE}/v1/me`, { headers: { Authorization: "Basic malformed" } })).status,
    ).toBe(401);
    expect(
      (await stub.fetch(`${BASE}/v1/me`, { headers: { Authorization: "Bearer unknown" } })).status,
    ).toBe(401);

    await runInDurableObject(stub, (instance) => {
      (instance as unknown as WorkspaceDO).store.sql.exec(
        `UPDATE users SET deactivated = 1 WHERE username = 'me-user'`,
      );
    });
    expect(
      (await stub.fetch(`${BASE}/v1/me`, { headers: { Authorization: `Bearer ${token}` } })).status,
    ).toBe(401);
  });
});
