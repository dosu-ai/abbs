import { createExecutionContext, env } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import worker from "../src/index";
import { runWorkspaceInventory } from "../src/inventory";
import { getWorkspace, relistWorkspace } from "../src/registry";
import { clearUpstreamCache } from "../src/upstream";
import { runVerificationSweep } from "../src/verify";
import {
  JSON_HEADERS,
  THREAD_ID,
  pageBody,
  seedWorkspace,
  serverInfoBody,
  threadBody,
} from "./helpers";
import { fetchMock } from "./mock";

const MESSAGE_ID = "0198aaaa-bbbb-7ccc-8ddd-eeeeffff9001";

function message(content = "Public content for crawler qualification"): Record<string, unknown> {
  return {
    id: MESSAGE_ID,
    thread_id: THREAD_ID,
    author: "ada",
    content,
    deleted: false,
    created_at: "2026-08-22T09:14:00Z",
    edited_at: null,
    seq: "188",
    reactions: [],
  };
}

function mockInventory(origin: string, anchor = "300"): void {
  const upstream = fetchMock.get(origin);
  upstream
    .intercept({ path: "/v1/threads", query: { limit: "100" } })
    .reply(200, pageBody([threadBody()], null, anchor), JSON_HEADERS);
  upstream
    .intercept({ path: "/v1/threads", query: { limit: "100", since: anchor } })
    .reply(200, pageBody([], null, String(Number(anchor) + 1)), JSON_HEADERS);
}

beforeEach(async () => {
  fetchMock.activate();
  fetchMock.reset();
  clearUpstreamCache();
  await env.DB.prepare("DELETE FROM public_thread_urls").run();
  await env.DB.prepare("DELETE FROM workspaces").run();
});

describe("search qualification", () => {
  it("starts unqualified and activates only after two spaced scheduled checks with content", async () => {
    const ws = await seedWorkspace({ slug: "qualify", baseUrl: "https://qualify.example" });
    const upstream = fetchMock.get(ws.baseUrl);
    upstream
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("qualify"), JSON_HEADERS)
      .times(2);
    upstream
      .intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, pageBody([threadBody()]), JSON_HEADERS)
      .times(2);
    upstream
      .intercept({ path: `/v1/threads/${THREAD_ID}/messages`, query: { limit: "5" } })
      .reply(200, pageBody([message()]), JSON_HEADERS)
      .times(2);
    mockInventory(ws.baseUrl);

    await runVerificationSweep(env, new Date("2026-08-22T00:00:00Z"));
    let row = await env.DB.prepare(
      "SELECT search_eligible, search_success_count, search_content_found FROM workspaces WHERE id = ?",
    )
      .bind(ws.id)
      .first<Record<string, number>>();
    expect(row).toEqual({ search_eligible: 0, search_success_count: 1, search_content_found: 1 });

    const second = await runVerificationSweep(env, new Date("2026-08-22T00:15:00Z"));
    expect(second.qualified).toBe(1);
    row = await env.DB.prepare(
      "SELECT search_eligible, search_success_count, search_content_found FROM workspaces WHERE id = ?",
    )
      .bind(ws.id)
      .first<Record<string, number>>();
    expect(row).toEqual({ search_eligible: 1, search_success_count: 2, search_content_found: 1 });
    expect(
      await env.DB.prepare("SELECT COUNT(*) AS n FROM public_thread_urls WHERE workspace_id = ?")
        .bind(ws.id)
        .first<number>("n"),
    ).toBe(1);
  });

  it("keeps an empty workspace unqualified, then activates when content appears later", async () => {
    const ws = await seedWorkspace({ slug: "later", baseUrl: "https://later.example" });
    const upstream = fetchMock.get(ws.baseUrl);
    upstream
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("later"), JSON_HEADERS)
      .times(3);
    upstream
      .intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, pageBody([]), JSON_HEADERS)
      .times(2);
    upstream
      .intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, pageBody([threadBody()]), JSON_HEADERS);
    upstream
      .intercept({ path: `/v1/threads/${THREAD_ID}/messages`, query: { limit: "5" } })
      .reply(200, pageBody([message()]), JSON_HEADERS);
    mockInventory(ws.baseUrl, "400");

    await runVerificationSweep(env, new Date("2026-08-22T00:00:00Z"));
    await runVerificationSweep(env, new Date("2026-08-22T00:15:00Z"));
    expect((await getWorkspace(env.DB, ws.slug))?.searchEligible).toBe(false);

    const third = await runVerificationSweep(env, new Date("2026-08-22T00:30:00Z"));
    expect(third.qualified).toBe(1);
    expect((await getWorkspace(env.DB, ws.slug))?.searchEligible).toBe(true);
  });

  it("suspends on deterministic failures but preserves eligibility through transient 5xx", async () => {
    const deterministic = await seedWorkspace({
      slug: "deterministic",
      baseUrl: "https://deterministic.example",
      searchEligible: true,
      searchSuccessCount: 2,
      searchContentFound: true,
    });
    fetchMock
      .get(deterministic.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(
        200,
        serverInfoBody("deterministic", { visibility: "private", directory_listing: false }),
        JSON_HEADERS,
      );
    const suspended = await runVerificationSweep(env, new Date("2026-08-22T01:00:00Z"));
    expect(suspended.suspended).toBe(1);
    expect((await getWorkspace(env.DB, deterministic.slug))?.searchEligible).toBe(false);

    await env.DB.prepare("DELETE FROM workspaces").run();
    const transient = await seedWorkspace({
      slug: "transient",
      baseUrl: "https://transient.example",
      searchEligible: true,
      searchSuccessCount: 2,
      searchContentFound: true,
    });
    fetchMock.get(transient.baseUrl).intercept({ path: "/v1/server" }).reply(503, "down");
    const kept = await runVerificationSweep(env, new Date("2026-08-22T01:15:00Z"));
    expect(kept.suspended).toBe(0);
    expect((await getWorkspace(env.DB, transient.slug))?.searchEligible).toBe(true);
  });

  it("delisting deletes URL inventory, returns 410, and relisting resets every search field", async () => {
    const ws = await seedWorkspace({
      slug: "gone-search",
      baseUrl: "https://gone-search.example",
      searchEligible: true,
      searchSuccessCount: 2,
      searchContentFound: true,
    });
    await env.DB.prepare(
      "INSERT INTO public_thread_urls (workspace_id, thread_id, discovered_at, last_seen_at) VALUES (?, ?, ?, ?)",
    )
      .bind(ws.id, THREAD_ID, "2026-08-22T00:00:00Z", "2026-08-22T00:00:00Z")
      .run();
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("gone-search", { directory_listing: false }), JSON_HEADERS);

    await runVerificationSweep(env, new Date("2026-08-22T02:00:00Z"));
    expect(
      await env.DB.prepare("SELECT COUNT(*) AS n FROM public_thread_urls WHERE workspace_id = ?")
        .bind(ws.id)
        .first<number>("n"),
    ).toBe(0);
    const gone = await worker.fetch(
      new Request("https://abbs.dev/w/gone-search"),
      env,
      createExecutionContext(),
    );
    expect(gone.status).toBe(410);

    await relistWorkspace(env.DB, ws.id);
    const relisted = await getWorkspace(env.DB, ws.slug);
    expect(relisted).toMatchObject({
      status: "pending",
      searchEligible: false,
      searchSuccessCount: 0,
      searchContentFound: false,
      inventoryPhase: "bootstrap",
      inventoryCursor: null,
      inventoryAnchor: null,
      inventoryCompletedAt: null,
    });
  });
});

describe("snapshot-and-tail URL inventory", () => {
  it("resumes after four pages, catches up from the bootstrap anchor, and upserts duplicates", async () => {
    const seeded = await seedWorkspace({
      slug: "inventory",
      baseUrl: "https://inventory.example",
      searchEligible: true,
      searchSuccessCount: 2,
      searchContentFound: true,
    });
    const ids = Array.from(
      { length: 6 },
      (_, i) => `0198aaaa-bbbb-7ccc-8ddd-${String(i + 1).padStart(12, "0")}`,
    );
    const upstream = fetchMock.get(seeded.baseUrl);
    upstream
      .intercept({ path: "/v1/threads", query: { limit: "100" } })
      .reply(200, pageBody([threadBody({ id: ids[0] })], "p2", "500"), JSON_HEADERS);
    for (let page = 2; page <= 4; page++) {
      upstream
        .intercept({ path: "/v1/threads", query: { page: `p${page}`, limit: "100" } })
        .reply(
          200,
          pageBody([threadBody({ id: ids[page - 1] })], `p${page + 1}`, "ignored"),
          JSON_HEADERS,
        );
    }

    let ws = await getWorkspace(env.DB, seeded.slug);
    expect(ws).not.toBeNull();
    const first = await runWorkspaceInventory(env.DB, ws!, "2026-08-22T03:00:00Z");
    expect(first).toMatchObject({ pages: 4, urls: 4, phase: "bootstrap", completed: false });
    ws = await getWorkspace(env.DB, seeded.slug);
    expect(ws).toMatchObject({ inventoryCursor: "p5", inventoryAnchor: "500" });

    upstream
      .intercept({ path: "/v1/threads", query: { page: "p5", limit: "100" } })
      .reply(200, pageBody([threadBody({ id: ids[4] })], null, "ignored"), JSON_HEADERS);
    upstream
      .intercept({ path: "/v1/threads", query: { limit: "100", since: "500" } })
      .reply(
        200,
        pageBody([threadBody({ id: ids[0] }), threadBody({ id: ids[5] })], null, "600"),
        JSON_HEADERS,
      );
    const second = await runWorkspaceInventory(env.DB, ws!, "2026-08-22T03:15:00Z");
    expect(second).toMatchObject({ pages: 2, urls: 3, phase: "incremental", completed: true });
    expect(
      await env.DB.prepare("SELECT COUNT(*) AS n FROM public_thread_urls WHERE workspace_id = ?")
        .bind(seeded.id)
        .first<number>("n"),
    ).toBe(6);
    const final = await getWorkspace(env.DB, seeded.slug);
    expect(final).toMatchObject({
      inventoryPhase: "incremental",
      inventoryCursor: null,
      inventoryAnchor: "600",
      inventoryCompletedAt: "2026-08-22T03:15:00Z",
    });
  });

  it("treats an anonymously returned private thread as a privacy failure", async () => {
    const ws = await seedWorkspace({
      slug: "leak-inventory",
      baseUrl: "https://leak-inventory.example",
      searchEligible: true,
    });
    fetchMock
      .get(ws.baseUrl)
      .intercept({ path: "/v1/threads", query: { limit: "100" } })
      .reply(200, pageBody([threadBody({ kind: "dm" })]), JSON_HEADERS);
    const result = await runWorkspaceInventory(env.DB, ws, "2026-08-22T04:00:00Z");
    expect(result.errorCode).toBe("private-thread-leak");
    expect(
      await env.DB.prepare("SELECT COUNT(*) AS n FROM public_thread_urls WHERE workspace_id = ?")
        .bind(ws.id)
        .first<number>("n"),
    ).toBe(0);
  });
});
