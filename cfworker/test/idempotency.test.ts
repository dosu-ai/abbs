// Idempotency middleware behaviors: byte-identical replay, route-pattern
// scope isolation, conflict on body mismatch, oversize keys, and purge at
// the 24h horizon (via direct created_ms manipulation — the black-box suite
// cannot move time).

import { SELF, env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { WorkspaceDO } from "../src/workspace-do";

const BASE = "http://abbs.test";

async function claim(username: string): Promise<string> {
  const resp = await SELF.fetch(`${BASE}/v1/users`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, kind: "agent" }),
  });
  expect(resp.status).toBe(201);
  const body = (await resp.json()) as { token: string };
  return body.token;
}

function post(path: string, token: string, body: unknown, key?: string): Promise<Response> {
  return SELF.fetch(`${BASE}${path}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
      ...(key ? { "Idempotency-Key": key } : {}),
    },
    body: JSON.stringify(body),
  });
}

describe("idempotency middleware", () => {
  it("replays the original response byte-for-byte", async () => {
    const token = await claim("idem-replay");
    const body = { title: "replay", content: "exactly once" };
    const first = await post("/v1/threads", token, body, "key-1");
    expect(first.status).toBe(201);
    const firstText = await first.text();

    const replay = await post("/v1/threads", token, body, "key-1");
    expect(replay.status).toBe(201);
    expect(await replay.text()).toBe(firstText);
  });

  it("conflicts when a key is reused with a different body", async () => {
    const token = await claim("idem-conflict");
    expect((await post("/v1/threads", token, { title: "a", content: "x" }, "key-2")).status).toBe(201);
    const conflict = await post("/v1/threads", token, { title: "b", content: "y" }, "key-2");
    expect(conflict.status).toBe(409);
    expect(await conflict.text()).toContain("idempotency-key-conflict");
  });

  it("scopes keys per route pattern, not per URL", async () => {
    const token = await claim("idem-scope");
    const t = await post("/v1/threads", token, { title: "scope", content: "root" }, "shared-key");
    expect(t.status).toBe(201);
    const thread = (await t.json()) as { id: string };

    // The same key on a different endpoint executes independently instead of
    // replaying the thread-creation response.
    const m = await post(`/v1/threads/${thread.id}/messages`, token, { content: "reply" }, "shared-key");
    expect(m.status).toBe(201);
    const msg = (await m.json()) as { thread_id: string; content: string };
    expect(msg.thread_id).toBe(thread.id);
    expect(msg.content).toBe("reply");
  });

  it("rejects oversize keys", async () => {
    const token = await claim("idem-oversize");
    const resp = await post("/v1/threads", token, { title: "k", content: "v" }, "k".repeat(129));
    expect(resp.status).toBe(400);
    expect(await resp.text()).toContain("validation");
  });

  it("replays the original response headers, not just the body", async () => {
    // Trip the loop guard (10 rapid ping-pong messages), then hit it with an
    // Idempotency-Key: the 429 carries Retry-After, and the replay must too.
    const a = await claim("idem-hdr-a");
    const b = await claim("idem-hdr-b");
    const t = await post("/v1/threads", a, { title: "loop", content: "0" });
    expect(t.status).toBe(201);
    const threadId = ((await t.json()) as { id: string }).id;
    for (let i = 1; i < 10; i++) {
      const r = await post(`/v1/threads/${threadId}/messages`, i % 2 === 0 ? a : b, { content: String(i) });
      expect(r.status).toBe(201);
    }
    const tripped = await post(`/v1/threads/${threadId}/messages`, a, { content: "tripped" }, "key-hdr");
    expect(tripped.status).toBe(429);
    const retryAfter = tripped.headers.get("Retry-After");
    expect(retryAfter).not.toBeNull();

    const replay = await post(`/v1/threads/${threadId}/messages`, a, { content: "tripped" }, "key-hdr");
    expect(replay.status).toBe(429);
    expect(replay.headers.get("Retry-After")).toBe(retryAfter);
    expect(await replay.text()).toBe(await tripped.text());
  });

  it("rejects a deactivated principal instead of replaying its cached response", async () => {
    const token = await claim("idem-deact");
    const body = { title: "deact", content: "cached" };
    expect((await post("/v1/threads", token, body, "key-deact")).status).toBe(201);

    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName(env.WORKSPACE_NAME ?? "abbs"));
    await runInDurableObject(stub, (instance) => {
      const d = instance as unknown as WorkspaceDO;
      d.store.sql.exec(`UPDATE users SET deactivated = 1 WHERE username = 'idem-deact'`);
    });

    // The exact same token/body/key must 401, never replay the cached 201.
    const resp = await post("/v1/threads", token, body, "key-deact");
    expect(resp.status).toBe(401);
  });

  it("re-executes once the record ages past the 24h horizon, and purges on write", async () => {
    const token = await claim("idem-purge");
    const body = { title: "purge", content: "expires" };
    const first = await post("/v1/threads", token, body, "key-3");
    expect(first.status).toBe(201);
    const firstId = ((await first.json()) as { id: string }).id;

    // Age every idempotency record past the retention horizon.
    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName(env.WORKSPACE_NAME ?? "abbs"));
    await runInDurableObject(stub, (instance) => {
      const d = instance as unknown as WorkspaceDO;
      d.store.sql.exec(`UPDATE idempotency SET created_ms = created_ms - ?`, 25 * 60 * 60 * 1000);
    });

    // The same key + body re-executes (a fresh thread), proving the aged
    // record no longer replays…
    const again = await post("/v1/threads", token, body, "key-3");
    expect(again.status).toBe(201);
    expect(((await again.json()) as { id: string }).id).not.toBe(firstId);

    // …and the write purged everything past the horizon: only the fresh
    // record remains.
    await runInDurableObject(stub, (instance) => {
      const d = instance as unknown as WorkspaceDO;
      const n = d.store.sql.exec(`SELECT COUNT(*) AS n FROM idempotency`).one().n as number;
      expect(n).toBe(1);
    });
  });
});
