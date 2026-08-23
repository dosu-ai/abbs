// The scheduled verification sweep: repeats discovery for every listed
// workspace, refreshes cached metadata, degrades or unreaches on failure,
// delists on lost directory_listing consent, and never contacts or
// resurrects a delisted row.

import { createExecutionContext, env } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import worker from "../src/index";
import { clearUpstreamCache } from "../src/upstream";
import { runVerificationSweep } from "../src/verify";
import { JSON_HEADERS, seedWorkspace, serverInfoBody } from "./helpers";
import { fetchMock } from "./mock";

async function statusOf(id: string): Promise<{ status: string; last_error_code: string | null } | null> {
  return env.DB.prepare("SELECT status, last_error_code FROM workspaces WHERE id = ?")
    .bind(id)
    .first();
}

beforeEach(async () => {
  fetchMock.activate();
  fetchMock.reset();
  clearUpstreamCache();
  // Sweep assertions count the whole registry; start each test from an
  // empty one (storage is per-file, not per-test).
  await env.DB.prepare("DELETE FROM workspaces").run();
});

describe("runVerificationSweep", () => {
  it("re-activates a pending listing and refreshes its cached metadata", async () => {
    const ws = await seedWorkspace({ slug: "sw-ok", baseUrl: "https://sw-ok.example", status: "pending" });
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("sw-fresh", { description: "freshly verified" }), JSON_HEADERS);
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, JSON.stringify({ items: [], next_page: null, as_of: "200" }), JSON_HEADERS);

    const summary = await runVerificationSweep(env);
    expect(summary).toMatchObject({ checked: 1, healthy: 1, delisted: 0 });

    const row = await env.DB.prepare("SELECT * FROM workspaces WHERE id = ?").bind(ws.id).first();
    expect(row?.status).toBe("active");
    expect(row?.name).toBe("sw-fresh");
    expect(row?.description).toBe("freshly verified");
    expect(row?.canonical_url).toBe("https://sw-fresh.example");
    expect(row?.last_success_at).toBe(row?.last_checked_at);
    fetchMock.assertNoPendingInterceptors();
  });

  it("delists on lost consent and never contacts the row again", async () => {
    const ws = await seedWorkspace({ slug: "sw-bye", baseUrl: "https://sw-bye.example" });
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("sw-bye", { directory_listing: false }), JSON_HEADERS);

    const first = await runVerificationSweep(env);
    expect(first.delisted).toBe(1);
    expect(await statusOf(ws.id)).toEqual({ status: "delisted", last_error_code: "listing-revoked" });

    // Second sweep: no mock exists for this origin — a contact would record
    // an unreachable failure. The row must be skipped and left untouched.
    const second = await runVerificationSweep(env);
    expect(second.checked).toBe(0);
    expect(await statusOf(ws.id)).toEqual({ status: "delisted", last_error_code: "listing-revoked" });
  });

  it("degrades on contract violations without delisting", async () => {
    const priv = await seedWorkspace({ slug: "sw-priv", baseUrl: "https://sw-priv.example" });
    const blank = await seedWorkspace({ slug: "sw-blank", baseUrl: "https://sw-blank.example" });
    fetchMock
      .get(priv.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("sw-priv", { visibility: "private", directory_listing: false }), JSON_HEADERS);
    fetchMock
      .get(blank.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("sw-blank", { description: "" }), JSON_HEADERS);

    const summary = await runVerificationSweep(env);
    expect(summary.degraded).toBe(2);
    // Private wins over missing consent: going private is a degradation,
    // not a listing withdrawal.
    expect(await statusOf(priv.id)).toEqual({ status: "degraded", last_error_code: "not-public" });
    expect(await statusOf(blank.id)).toEqual({ status: "degraded", last_error_code: "no-description" });
  });

  it("marks a dead upstream unreachable but keeps the listing", async () => {
    const ws = await seedWorkspace({ slug: "sw-dead", baseUrl: "https://sw-dead.example" });
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/server" })
      .replyWithError(new Error("connect failed"));

    const summary = await runVerificationSweep(env);
    expect(summary.unreachable).toBe(1);
    expect(await statusOf(ws.id)).toEqual({ status: "unreachable", last_error_code: "network" });
  });

  it("supports the operator relist path: delisted -> pending -> active", async () => {
    const ws = await seedWorkspace({ slug: "sw-back", baseUrl: "https://sw-back.example", status: "delisted" });
    // The documented relist statement (web/README.md).
    await env.DB.prepare(
      "UPDATE workspaces SET status = 'pending', last_error_code = NULL WHERE id = ? AND status = 'delisted'",
    )
      .bind(ws.id)
      .run();
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("sw-back"), JSON_HEADERS);
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, JSON.stringify({ items: [], next_page: null, as_of: "200" }), JSON_HEADERS);

    const summary = await runVerificationSweep(env);
    expect(summary.healthy).toBe(1);
    expect((await statusOf(ws.id))?.status).toBe("active");
  });
});

describe("scheduled handler", () => {
  it("runs the sweep from the cron trigger", async () => {
    const ws = await seedWorkspace({ slug: "sw-cron", baseUrl: "https://sw-cron.example", status: "pending" });
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("sw-cron"), JSON_HEADERS);
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, JSON.stringify({ items: [], next_page: null, as_of: "200" }), JSON_HEADERS);

    const controller = {
      scheduledTime: Date.now(),
      cron: "*/15 * * * *",
      noRetry(): void {},
    } as ScheduledController;
    await worker.scheduled(controller, env, createExecutionContext());
    expect((await statusOf(ws.id))?.status).toBe("active");
  });
});
