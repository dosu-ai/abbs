// The constrained read proxy: allowlist, validation, limits, typed errors,
// and the short cache. All network is mocked; disableNetConnect() makes any
// unexpected outbound request a test failure.

import { afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";
import type { RegistryWorkspace } from "../src/types";
import {
  clearUpstreamCache,
  fetchDiscovery,
  fetchMessages,
  fetchThread,
  fetchThreads,
  fetchUser,
  validatePageParams,
} from "../src/upstream";
import { JSON_HEADERS, THREAD_ID, pageBody, threadBody } from "./helpers";
import { fetchMock } from "./mock";

function ws(over: Partial<RegistryWorkspace> = {}): RegistryWorkspace {
  return {
    id: over.id ?? "0198c0de-1111-7000-8000-000000000001",
    slug: "up",
    baseUrl: over.baseUrl ?? "https://up.example",
    canonicalUrl: null,
    name: "up",
    description: "",
    apiVersion: null,
    status: "active",
    submittedAt: "2026-08-22T00:00:00Z",
    lastCheckedAt: null,
    lastSuccessAt: null,
    lastErrorCode: null,
    ...over,
  };
}

beforeAll(() => {
  fetchMock.activate();
  fetchMock.disableNetConnect();
});

beforeEach(() => clearUpstreamCache());
afterEach(() => {
  fetchMock.assertNoPendingInterceptors();
  fetchMock.reset();
});

describe("validation before any network", () => {
  it("rejects a non-UUID thread id locally", async () => {
    const r = await fetchThread(ws(), "../../v1/server");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("not-found");
  });

  it("rejects an invalid username locally", async () => {
    const r = await fetchUser(ws(), "No/Slash");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("not-found");
  });

  it("refuses plain-http origins that are not loopback", async () => {
    const r = await fetchDiscovery(ws({ baseUrl: "http://up.example" }));
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("insecure-url");
  });

  it("validatePageParams enforces the contract", () => {
    expect(validatePageParams({ page: "tok", limit: "50" })).toEqual({ page: "tok", limit: 50 });
    expect(validatePageParams({ limit: "0" })).toBeNull();
    expect(validatePageParams({ limit: "101" })).toBeNull();
    expect(validatePageParams({ limit: "abc" })).toBeNull();
    expect(validatePageParams({ page: "x".repeat(257) })).toBeNull();
    expect(validatePageParams({ tags: Array.from({ length: 17 }, (_, i) => `t${i}`) })).toBeNull();
    expect(validatePageParams({ tags: ["x".repeat(65)] })).toBeNull();
    expect(validatePageParams({ tags: ["ok"] })).toEqual({ tags: ["ok"] });
  });
});

describe("responses and typed errors", () => {
  it("returns protocol-shaped thread pages", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/threads" })
      .reply(200, pageBody([threadBody()], "next-tok"), JSON_HEADERS);
    const r = await fetchThreads(ws(), {});
    expect(r.ok).toBe(true);
    if (r.ok) {
      expect(r.value.items[0].title).toBe("Replace polling with websocket");
      expect(r.value.next_page).toBe("next-tok");
      expect(r.fresh).toBe(true);
    }
  });

  it("serves the second read from cache", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/threads" })
      .reply(200, pageBody([threadBody()]), JSON_HEADERS);
    const first = await fetchThreads(ws(), {});
    const second = await fetchThreads(ws(), {});
    expect(first.ok && first.fresh).toBe(true);
    expect(second.ok).toBe(true);
    if (second.ok) expect(second.fresh).toBe(false);
  });

  it("refresh bypasses the cache", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/threads" })
      .reply(200, pageBody([threadBody()]), JSON_HEADERS)
      .times(2);
    const first = await fetchThreads(ws(), {});
    const second = await fetchThreads(ws(), {}, true);
    expect(first.ok && first.fresh).toBe(true);
    expect(second.ok && second.fresh).toBe(true);
  });

  it("maps upstream 404 to not-found", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: `/v1/threads/${THREAD_ID}` })
      .reply(404, JSON.stringify({ type: "x", title: "x", status: 404 }), JSON_HEADERS);
    const r = await fetchThread(ws(), THREAD_ID);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("not-found");
  });

  it("maps upstream 5xx to http-5xx (degraded)", async () => {
    fetchMock.get("https://up.example").intercept({ path: "/v1/threads" }).reply(503, "oops");
    const r = await fetchThreads(ws(), {});
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("http-5xx");
  });

  it("honors 429 with Retry-After", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/threads" })
      .reply(429, "slow down", { headers: { "Retry-After": "17" } });
    const r = await fetchThreads(ws(), {});
    expect(r.ok).toBe(false);
    if (!r.ok) {
      expect(r.code).toBe("rate-limited");
      expect(r.retryAfterSeconds).toBe(17);
    }
  });

  it("treats redirects as errors, never follows them", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/server" })
      .reply(302, "", { headers: { Location: "https://evil.example/v1/server" } });
    const r = await fetchDiscovery(ws());
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("redirect");
  });

  it("rejects non-JSON content types without reflecting the body", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/server" })
      .reply(200, "<html>upstream junk</html>", { headers: { "Content-Type": "text/html" } });
    const r = await fetchDiscovery(ws());
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("bad-content-type");
  });

  it("rejects malformed JSON", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/server" })
      .reply(200, "{not json", JSON_HEADERS);
    const r = await fetchDiscovery(ws());
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("bad-json");
  });

  it("rejects protocol-shaped-but-wrong payloads", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/threads" })
      .reply(200, JSON.stringify({ items: "nope", next_page: null, as_of: "1" }), JSON_HEADERS);
    const r = await fetchThreads(ws(), {});
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("bad-schema");
  });

  it("caps oversized bodies", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/threads" })
      .reply(200, `{"items": ["${"x".repeat(1_100_000)}"]}`, JSON_HEADERS);
    const r = await fetchThreads(ws(), {});
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.code).toBe("too-large");
  });

  it("accepts tombstoned messages without content", async () => {
    const tomb = {
      id: "0198aaaa-bbbb-7ccc-8ddd-eeeeffff0002",
      thread_id: THREAD_ID,
      author: "ada",
      deleted: true,
      created_at: "2026-08-22T09:00:00Z",
      deleted_at: "2026-08-22T10:00:00Z",
      deleted_by: "ada",
      seq: "191",
      reactions: [],
    };
    fetchMock
      .get("https://up.example")
      .intercept({ path: `/v1/threads/${THREAD_ID}/messages` })
      .reply(200, pageBody([tomb]), JSON_HEADERS);
    const r = await fetchMessages(ws(), THREAD_ID, {});
    expect(r.ok).toBe(true);
    if (r.ok) {
      expect(r.value.items[0].deleted).toBe(true);
      expect(r.value.items[0].content).toBeUndefined();
    }
  });

  it("sends tag, page, and limit as upstream query parameters", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/threads", query: { tag: "api", page: "tok", limit: "25" } })
      .reply(200, pageBody([]), JSON_HEADERS);
    const r = await fetchThreads(ws(), { tags: ["api"], page: "tok", limit: 25 });
    expect(r.ok).toBe(true);
  });

  it("flags a private workspace as not-public via discovery state", async () => {
    fetchMock
      .get("https://up.example")
      .intercept({ path: "/v1/server" })
      .reply(
        200,
        JSON.stringify({
          api_version: "v1",
          workspace: { name: "up", visibility: "private", directory_listing: false },
          auth_modes: ["first-claim"],
          limits: {},
        }),
        JSON_HEADERS,
      );
    const r = await fetchDiscovery(ws());
    // Discovery itself succeeds; policy interpretation lives in health.ts.
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.value.workspace.visibility).toBe("private");
  });
});
