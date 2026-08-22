// Phase 3 registration: URL normalization, the idempotent POST
// /api/workspaces, the no-JS /add form flow, verification failures with
// precise bounded errors, the delist invariant, and per-address rate limits.

import { createExecutionContext, env, waitOnExecutionContext } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import worker from "../src/index";
import { normalizeSubmission } from "../src/register";
import type { NormalizeReject } from "../src/register";
import { clearUpstreamCache } from "../src/upstream";
import { JSON_HEADERS, THREAD_ID, pageBody, seedWorkspace, serverInfoBody, threadBody } from "./helpers";
import { fetchMock } from "./mock";

async function dispatch(path: string, init: RequestInit): Promise<Response> {
  const ctx = createExecutionContext();
  const resp = await worker.fetch(new Request(`https://abbs.dev${path}`, init), env, ctx);
  await waitOnExecutionContext(ctx);
  return resp;
}

let addrCounter = 0;
function freshAddr(): string {
  addrCounter++;
  return `203.0.113.${addrCounter}`;
}

function postApi(url: string, addr = freshAddr()): Promise<Response> {
  return dispatch("/api/workspaces", {
    method: "POST",
    headers: { "Content-Type": "application/json", "CF-Connecting-IP": addr },
    body: JSON.stringify({ url }),
  });
}

function postForm(url: string, addr = freshAddr()): Promise<Response> {
  return dispatch("/add", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", "CF-Connecting-IP": addr },
    body: new URLSearchParams({ url }).toString(),
  });
}

// A fully conforming workspace at `origin`: discovery (persisted, so later
// page renders can re-read it), one thread-list probe, one message-list
// probe. The declared canonical URL must equal the submitted origin.
function mockConforming(origin: string, name: string): void {
  const o = fetchMock.get(origin);
  o.intercept({ path: "/v1/server" })
    .reply(200, serverInfoBody(name, { canonical_url: origin }), JSON_HEADERS)
    .persist();
  o.intercept({ path: "/v1/threads", query: { limit: "5" } })
    .reply(200, pageBody([threadBody()]), JSON_HEADERS);
  o.intercept({ path: `/v1/threads/${THREAD_ID}/messages`, query: { limit: "1" } })
    .reply(200, pageBody([]), JSON_HEADERS);
}

async function selectRow(baseUrl: string): Promise<Record<string, unknown> | null> {
  return env.DB.prepare("SELECT * FROM workspaces WHERE base_url = ?").bind(baseUrl).first();
}

beforeEach(() => {
  fetchMock.activate();
  fetchMock.reset();
  clearUpstreamCache();
});

describe("normalizeSubmission", () => {
  it("normalizes bare hostnames, case, trailing slash, and trailing dot", () => {
    const cases: [string, string][] = [
      ["https://Bbs.Example.Com", "https://bbs.example.com"],
      ["  bbs.example.com/  ", "https://bbs.example.com"],
      ["https://bbs.example.com.", "https://bbs.example.com"],
      ["https://xn--bcher-kva.example.com", "https://xn--bcher-kva.example.com"],
    ];
    for (const [raw, origin] of cases) {
      expect(normalizeSubmission(raw)).toEqual({ ok: true, origin });
    }
  });

  it("rejects everything that is not a plain public HTTPS origin", () => {
    const cases: [string, NormalizeReject][] = [
      ["", "empty"],
      ["   ", "empty"],
      [`https://${"a".repeat(520)}.com`, "too-long"],
      ["not a url", "unparseable"],
      ["http://bbs.example.com", "not-https"],
      ["ftp://bbs.example.com", "not-https"],
      ["https://user:pw@bbs.example.com", "credentials"],
      ["https://bbs.example.com#v1", "fragment"],
      ["https://bbs.example.com?x=1", "query"],
      ["https://bbs.example.com/v1", "non-root-path"],
      ["https://bbs.example.com:8443", "explicit-port"],
      ["https://127.0.0.1", "ip-literal"],
      ["https://127.1", "ip-literal"],
      ["https://2130706433", "ip-literal"],
      ["https://0x7f000001", "ip-literal"],
      ["https://[::1]", "ip-literal"],
      ["https://intranet", "host-not-public"],
      ["https://localhost", "host-not-public"],
      ["https://board.local", "host-not-public"],
      ["https://svc.internal", "host-not-public"],
      ["https://gateway.home.arpa", "host-not-public"],
      ["https://bbs.test", "host-not-public"],
      ["https://bbs.example", "host-not-public"],
      ["https://hidden.onion", "host-not-public"],
    ];
    for (const [raw, code] of cases) {
      expect(normalizeSubmission(raw), raw).toEqual({ ok: false, code });
    }
  });
});

describe("POST /api/workspaces", () => {
  it("registers a conforming workspace once, then answers idempotently", async () => {
    mockConforming("https://board-one.dev", "one-name");

    const first = await postApi("board-one.dev");
    expect(first.status).toBe(201);
    expect(first.headers.get("Location")).toBe("/w/one-name");
    const created = (await first.json()) as { url: string; workspace: { slug: string; status: string } };
    expect(created.url).toBe("/w/one-name");
    expect(created.workspace.status).toBe("active");

    const row = await selectRow("https://board-one.dev");
    expect(row?.status).toBe("active");
    expect(row?.name).toBe("one-name");
    expect(row?.description).toBe("one-name description");
    expect(row?.canonical_url).toBe("https://board-one.dev");
    expect(row?.api_version).toBe("v1");
    // Exactly one thread probe and one message probe went out.
    fetchMock.assertNoPendingInterceptors();

    // Resubmission returns the existing listing without spending rate
    // budget or re-probing (no interceptors remain — a probe would fail).
    const again = await postApi("https://board-one.dev/");
    expect(again.status).toBe(200);
    const body = (await again.json()) as { url: string };
    expect(body.url).toBe("/w/one-name");

    // And the board is live in the directory.
    const dir = await dispatch("/", { method: "GET" });
    expect(await dir.text()).toContain(`href="/w/one-name"`);
  });

  it("skips the message probe when the public thread list is empty", async () => {
    const origin = "https://empty-board.dev";
    const o = fetchMock.get(origin);
    o.intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("empty-board", { canonical_url: origin }), JSON_HEADERS);
    o.intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, pageBody([]), JSON_HEADERS);
    const r = await postApi(origin);
    expect(r.status).toBe(201);
    fetchMock.assertNoPendingInterceptors();
  });

  it("resolves display-name slug collisions deterministically", async () => {
    for (const origin of ["https://twin-a.dev", "https://twin-b.dev"]) {
      const o = fetchMock.get(origin);
      o.intercept({ path: "/v1/server" })
        .reply(200, serverInfoBody("Same Name!", { canonical_url: origin }), JSON_HEADERS);
      o.intercept({ path: "/v1/threads", query: { limit: "5" } })
        .reply(200, pageBody([]), JSON_HEADERS);
    }
    expect((await postApi("https://twin-a.dev")).headers.get("Location")).toBe("/w/same-name");
    expect((await postApi("https://twin-b.dev")).headers.get("Location")).toBe("/w/same-name-2");
  });

  it("rejects malformed URLs with a validation problem and no upstream contact", async () => {
    const r = await postApi("http://plain.dev");
    expect(r.status).toBe(400);
    const body = (await r.json()) as { type: string; detail: string };
    expect(body.type).toContain("validation");
    expect(body.detail).toContain("HTTPS");
  });

  const contractCases: [string, Record<string, unknown>, string][] = [
    ["private visibility", { visibility: "private" }, "not-public"],
    ["missing listing consent", { directory_listing: false }, "listing-revoked"],
    ["empty description", { description: "  " }, "no-description"],
    ["missing canonical URL", { canonical_url: undefined }, "bad-canonical"],
    ["non-HTTPS canonical URL", { canonical_url: "http://x.dev" }, "bad-canonical"],
  ];
  for (const [label, extra, code] of contractCases) {
    it(`refuses a workspace with ${label}`, async () => {
      const origin = `https://c-${code}.dev`;
      fetchMock
        .get(origin)
        .intercept({ path: "/v1/server" })
        .reply(200, serverInfoBody("c-board", extra), JSON_HEADERS);
      const r = await postApi(origin);
      expect(r.status).toBe(422);
      const body = (await r.json()) as { type: string; detail: string };
      expect(body.type).toContain("registration-failed");
      expect(body.detail).toContain(code);
      expect(await selectRow(origin)).toBeNull();
    });
  }

  it("refuses a canonical URL naming a different origin, and says which", async () => {
    const origin = "https://mirror.dev";
    fetchMock
      .get(origin)
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("real", { canonical_url: "https://real.dev" }), JSON_HEADERS);
    const r = await postApi(origin);
    expect(r.status).toBe(422);
    const body = (await r.json()) as { detail: string };
    expect(body.detail).toContain("canonical-mismatch");
    expect(body.detail).toContain("https://real.dev");
  });

  it("refuses the wrong protocol version", async () => {
    const origin = "https://v2-board.dev";
    fetchMock
      .get(origin)
      .intercept({ path: "/v1/server" })
      .reply(
        200,
        JSON.stringify({
          api_version: "v2",
          workspace: { name: "v2-board", description: "d", visibility: "public", canonical_url: origin, directory_listing: true },
        }),
        JSON_HEADERS,
      );
    const r = await postApi(origin);
    expect(r.status).toBe(422);
    expect(((await r.json()) as { detail: string }).detail).toContain("wrong-api-version");
  });

  it("refuses when discovery challenges or the anonymous probes fail", async () => {
    fetchMock
      .get("https://auth-disc.dev")
      .intercept({ path: "/v1/server" })
      .reply(401, "", JSON_HEADERS);
    let r = await postApi("https://auth-disc.dev");
    expect(r.status).toBe(422);
    expect(((await r.json()) as { detail: string }).detail).toContain("discovery: http-4xx");

    const threads401 = fetchMock.get("https://auth-threads.dev");
    threads401
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("t", { canonical_url: "https://auth-threads.dev" }), JSON_HEADERS);
    threads401.intercept({ path: "/v1/threads", query: { limit: "5" } }).reply(401, "", JSON_HEADERS);
    r = await postApi("https://auth-threads.dev");
    expect(r.status).toBe(422);
    expect(((await r.json()) as { detail: string }).detail).toContain("thread-list: http-4xx");

    const msg404 = fetchMock.get("https://msg-404.dev");
    msg404
      .intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("m", { canonical_url: "https://msg-404.dev" }), JSON_HEADERS);
    msg404
      .intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, pageBody([threadBody()]), JSON_HEADERS);
    msg404
      .intercept({ path: `/v1/threads/${THREAD_ID}/messages`, query: { limit: "1" } })
      .reply(401, "", JSON_HEADERS);
    r = await postApi("https://msg-404.dev");
    expect(r.status).toBe(422);
    expect(((await r.json()) as { detail: string }).detail).toContain("message-list: http-4xx");
  });

  it("refuses an anonymous thread list that leaks a non-public thread", async () => {
    const origin = "https://leaky.dev";
    const o = fetchMock.get(origin);
    o.intercept({ path: "/v1/server" })
      .reply(200, serverInfoBody("leaky", { canonical_url: origin }), JSON_HEADERS);
    o.intercept({ path: "/v1/threads", query: { limit: "5" } })
      .reply(200, pageBody([threadBody({ kind: "dm" })]), JSON_HEADERS);
    const r = await postApi(origin);
    expect(r.status).toBe(422);
    expect(((await r.json()) as { detail: string }).detail).toContain("private-thread-leak");
    expect(await selectRow(origin)).toBeNull();
  });

  it("never resurrects a delisted workspace", async () => {
    const gone = await seedWorkspace({ baseUrl: "https://gone.dev", status: "delisted" });
    const r = await postApi("https://gone.dev");
    expect(r.status).toBe(409);
    expect(((await r.json()) as { type: string }).type).toContain("delisted");
    const row = await selectRow(gone.baseUrl);
    expect(row?.status).toBe("delisted");
  });

  it("rate limits verification attempts per address", async () => {
    const addr = freshAddr();
    // No mocks: each attempt burns a credit and fails as unreachable.
    for (let i = 1; i <= 3; i++) {
      const r = await postApi(`https://rl-${i}.dev`, addr);
      expect(r.status).toBe(422);
    }
    const limited = await postApi("https://rl-4.dev", addr);
    expect(limited.status).toBe(429);
    expect(limited.headers.get("Retry-After")).toBe("300");
    // A different address is unaffected.
    expect((await postApi("https://rl-5.dev")).status).toBe(422);
  });

  it("rejects bodies that are not JSON-with-url or a url form field", async () => {
    const cases: RequestInit[] = [
      { method: "POST", body: JSON.stringify({ url: "https://x.dev" }) }, // no content type
      { method: "POST", headers: { "Content-Type": "application/json" }, body: "{nope" },
      { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ url: 7 }) },
    ];
    for (const init of cases) {
      const r = await dispatch("/api/workspaces", init);
      expect(r.status).toBe(400);
    }
    const huge = await dispatch("/api/workspaces", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url: "x".repeat(5000) }),
    });
    expect(huge.status).toBe(400);
  });
});

describe("POST /add (no-JS form flow)", () => {
  it("redirects to the new listing after a successful submission", async () => {
    mockConforming("https://form-board.dev", "form-board");
    const r = await postForm("form-board.dev");
    expect(r.status).toBe(303);
    expect(r.headers.get("Location")).toBe("https://abbs.dev/w/form-board");
  });

  it("re-renders the form with a precise error and the value preserved", async () => {
    const r = await postForm("http://plain-form.dev");
    expect(r.status).toBe(400);
    const html = await r.text();
    expect(html).toContain("PUBLIC BOARDS MUST BE SERVED OVER HTTPS.");
    expect(html).toContain(`value="http://plain-form.dev"`);
    expect(html).toContain(`<form method="post" action="/add"`);
  });

  it("escapes the re-rendered submission value", async () => {
    const r = await postForm(`https://a"b.dev`);
    expect(r.status).toBe(400);
    const html = await r.text();
    expect(html).toContain("&quot;b.dev");
    expect(html).not.toContain(`a"b.dev`);
  });

  it("explains a delisted workspace instead of relisting it", async () => {
    await seedWorkspace({ baseUrl: "https://form-gone.dev", status: "delisted" });
    const r = await postForm("https://form-gone.dev");
    expect(r.status).toBe(409);
    expect(await r.text()).toContain("REMOVED BY THE DIRECTORY OPERATORS");
  });

  it("serves the form on GET /add", async () => {
    const r = await dispatch("/add", { method: "GET" });
    expect(r.status).toBe(200);
    const html = await r.text();
    expect(html).toContain(`<form method="post" action="/add"`);
    expect(html).toContain("directory_listing");
  });
});
