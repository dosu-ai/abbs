// The lost-wakeup test: park a long-poll, append a matching event *between*
// its query and its park via the instrumented hook, and assert it wakes.
// The hook also asserts the poll had already subscribed before querying —
// the ordering that makes the window safe — so this never sleeps-and-hopes.

import { env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { parseWorkspaceConfig } from "../src/config";
import type { WorkspaceDO } from "../src/workspace-do";
import { postMessage } from "../src/store/messages";

const BASE = "http://abbs.test";

describe("events long-poll", () => {
  it("wakes when an event is appended between query and park", async () => {
    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName(parseWorkspaceConfig(env).id));

    const claim = async (username: string) => {
      const resp = await stub.fetch(`${BASE}/v1/users`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, kind: "agent" }),
      });
      expect(resp.status).toBe(201);
      return ((await resp.json()) as { token: string }).token;
    };
    const alice = await claim("wake-alice");
    const bob = await claim("wake-bob");

    const created = await stub.fetch(`${BASE}/v1/threads`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${alice}` },
      body: JSON.stringify({ title: "wakeups", content: "start" }),
    });
    expect(created.status).toBe(201);
    const thread = (await created.json()) as { id: string; last_activity_seq: string };

    // Bob is caught up: his cursor is the newest event.
    const drained = await stub.fetch(`${BASE}/v1/events?limit=100`, {
      headers: { Authorization: `Bearer ${bob}` },
    });
    const { cursor } = (await drained.json()) as { cursor: string };

    // Arm the hook: the moment bob's poll finishes its (empty) query and is
    // about to park, append a message directly through the store — the
    // append lands exactly inside the query→park window.
    const observed = { fired: false, waitersAtHook: -1 };
    await runInDurableObject(stub, (instance) => {
      const d = instance as unknown as WorkspaceDO;
      d.testHooks.afterEventsQuery = () => {
        d.testHooks.afterEventsQuery = undefined;
        observed.fired = true;
        observed.waitersAtHook = d.waiterCount(); // proves the poll subscribed before querying
        postMessage(d.store, thread.id, "wake-alice", "wake up", Date.now());
      };
    });

    const start = Date.now();
    const resp = await stub.fetch(`${BASE}/v1/events?timeout=30&cursor=${cursor}`, {
      headers: { Authorization: `Bearer ${bob}` },
    });
    expect(resp.status).toBe(200);
    const batch = (await resp.json()) as { events: Array<Record<string, unknown>>; cursor: string };

    expect(observed.fired).toBe(true);
    expect(observed.waitersAtHook).toBe(1); // subscribed-before-query held
    expect(batch.events.length).toBe(1);
    expect(batch.events[0].type).toBe("message.created");
    // A lost wakeup would hold the full 30s timeout; the woken poll returns
    // promptly (bounded generously below the vitest timeout).
    expect(Date.now() - start).toBeLessThan(5000);
  });

  it("echoes the cursor on an empty timed poll", async () => {
    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName(parseWorkspaceConfig(env).id));
    const resp = await stub.fetch(`${BASE}/v1/users`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: "echo-user", kind: "agent" }),
    });
    const { token } = (await resp.json()) as { token: string };

    const drained = await stub.fetch(`${BASE}/v1/events?limit=100`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    const { cursor } = (await drained.json()) as { cursor: string };

    const start = Date.now();
    const held = await stub.fetch(`${BASE}/v1/events?timeout=1&cursor=${cursor}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    const batch = (await held.json()) as { events: unknown[]; cursor: string };
    expect(batch.events).toEqual([]);
    expect(batch.cursor).toBe(cursor);
    expect(Date.now() - start).toBeGreaterThanOrEqual(1000);
  });
});
