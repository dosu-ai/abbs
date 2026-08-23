import { createExecutionContext, env } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import worker from "../src/index";
import { page } from "../src/layout";
import { discussionStructuredData } from "../src/seo";
import type { UpstreamMessage, UpstreamThread } from "../src/types";
import { clearUpstreamCache } from "../src/upstream";
import {
  JSON_HEADERS,
  THREAD_ID,
  pageBody,
  seedWorkspace,
  serverInfoBody,
  threadBody,
} from "./helpers";
import { fetchMock } from "./mock";

const OPENING_ID = "0198aaaa-bbbb-7ccc-8ddd-eeeeffff8101";
const REPLY_ID = "0198aaaa-bbbb-7ccc-8ddd-eeeeffff8102";

async function site(path: string, init?: RequestInit): Promise<Response> {
  return worker.fetch(new Request(`https://abbs.dev${path}`, init), env, createExecutionContext());
}

function message(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: OPENING_ID,
    thread_id: THREAD_ID,
    author: "ada",
    content:
      "**Public agents** coordinate durable work here with enough detail to produce a useful search description for visitors.",
    deleted: false,
    created_at: "2026-08-22T09:14:00Z",
    edited_at: null,
    seq: "188",
    reactions: [{ emoji: "👍", count: 2 }],
    ...over,
  };
}

beforeEach(async () => {
  fetchMock.activate();
  fetchMock.reset();
  clearUpstreamCache();
  await env.DB.prepare("DELETE FROM public_thread_urls").run();
  await env.DB.prepare("DELETE FROM workspaces").run();
});

async function seedSeoFixtures(): Promise<void> {
  const eligible = await seedWorkspace({
    slug: "seo-board",
    baseUrl: "https://seo-board.example",
    name: "SEO Board",
    description: "Directory of public agent bulletin board system.",
    searchEligible: true,
    searchSuccessCount: 2,
    searchContentFound: true,
  });
  const pending = await seedWorkspace({
    slug: "pending-board",
    baseUrl: "https://pending-board.example",
    name: "Pending Board",
  });
  await env.DB.prepare(
    "INSERT INTO public_thread_urls (workspace_id, thread_id, discovered_at, last_seen_at) VALUES (?, ?, ?, ?)",
  )
    .bind(eligible.id, THREAD_ID, "2026-08-22T10:00:00Z", "2026-08-22T10:00:00Z")
    .run();

  const seo = fetchMock.get(eligible.baseUrl);
  seo
    .intercept({ path: "/v1/server" })
    .reply(
      200,
      serverInfoBody("SEO Board", { description: "Directory of public agent bulletin board system." }),
      JSON_HEADERS,
    )
    .persist();
  seo
    .intercept({ path: "/v1/threads", query: { limit: "50" } })
    .reply(200, pageBody([threadBody()]), JSON_HEADERS)
    .persist();
  seo
    .intercept({ path: "/v1/tags", query: { limit: "50" } })
    .reply(200, pageBody([{ name: "api", thread_count: 1 }]), JSON_HEADERS)
    .persist();
  seo
    .intercept({ path: `/v1/threads/${THREAD_ID}` })
    .reply(200, JSON.stringify(threadBody()), JSON_HEADERS)
    .persist();
  seo
    .intercept({ path: `/v1/threads/${THREAD_ID}/messages`, query: { limit: "50" } })
    .reply(
      200,
      pageBody([
        message(),
        message({
          id: REPLY_ID,
          author: "buildbot",
          content: "The agent-authored reply is visible and attributed.",
          created_at: "2026-08-22T09:22:00Z",
          edited_at: "2026-08-22T09:25:00Z",
          seq: "193",
          reactions: [{ emoji: "👎", count: 1 }],
        }),
        message({
          id: "0198aaaa-bbbb-7ccc-8ddd-eeeeffff8103",
          author: "lin",
          deleted: true,
          content: undefined,
          deleted_at: "2026-08-22T09:40:00Z",
          deleted_by: "lin",
        }),
      ]),
      JSON_HEADERS,
    )
    .persist();
  seo
    .intercept({ path: `/v1/threads/${THREAD_ID}/messages`, query: { page: "reply-page", limit: "50" } })
    .reply(
      200,
      pageBody([
        message({
          id: "0198aaaa-bbbb-7ccc-8ddd-eeeeffff8110",
          author: "buildbot",
          content: "A reply on a real paginated page.",
        }),
      ]),
      JSON_HEADERS,
    )
    .persist();
  seo
    .intercept({ path: `/v1/threads/${THREAD_ID}/messages`, query: { limit: "1" } })
    .reply(200, pageBody([message()]), JSON_HEADERS)
    .persist();
  for (const [username, kind] of [
    ["ada", "human"],
    ["buildbot", "agent"],
    ["lin", "human"],
  ] as const) {
    seo
      .intercept({ path: `/v1/users/${username}` })
      .reply(200, JSON.stringify({ username, kind }), JSON_HEADERS)
      .persist();
  }

  const waiting = fetchMock.get(pending.baseUrl);
  waiting
    .intercept({ path: "/v1/server" })
    .reply(200, serverInfoBody("Pending Board"), JSON_HEADERS)
    .persist();
  waiting
    .intercept({ path: "/v1/threads", query: { limit: "50" } })
    .reply(200, pageBody([threadBody()]), JSON_HEADERS)
    .persist();
  waiting
    .intercept({ path: "/v1/tags", query: { limit: "50" } })
    .reply(200, pageBody([]), JSON_HEADERS)
    .persist();
}

describe("crawler interfaces", () => {
  it("serves the exact robots policy and the static 1200×630 social asset", async () => {
    expect(await (await site("/robots.txt")).text()).toBe(
      "User-agent: *\nAllow: /\nDisallow: /api/\nSitemap: https://abbs.dev/sitemap.xml\n",
    );
    const image = await site("/social-preview.png");
    expect(image.status).toBe(200);
    expect(image.headers.get("Content-Type")).toBe("image/png");
    expect((await image.arrayBuffer()).byteLength).toBeGreaterThan(10_000);
  });

  it("builds gated sitemap indexes and workspace URL chunks without lastmod", async () => {
    await seedSeoFixtures();
    const index = await site("/sitemap.xml");
    expect(index.status).toBe(200);
    expect(index.headers.get("Cache-Control")).toContain("max-age=900");
    const indexXml = await index.text();
    expect(indexXml).toContain("https://abbs.dev/sitemaps/site.xml");
    expect(indexXml).toContain("https://abbs.dev/sitemaps/w/seo-board/1.xml");
    expect(indexXml).not.toContain("pending-board");

    const staticXml = await (await site("/sitemaps/site.xml")).text();
    expect(staticXml).toContain("<loc>https://abbs.dev/</loc>");
    expect(staticXml).toContain("<loc>https://abbs.dev/help</loc>");
    expect(staticXml).not.toContain("/add");

    const workspaceXml = await (await site("/sitemaps/w/seo-board/1.xml")).text();
    expect(workspaceXml).toContain("<loc>https://abbs.dev/w/seo-board</loc>");
    expect(workspaceXml).toContain(`<loc>https://abbs.dev/w/seo-board/t/${THREAD_ID}</loc>`);
    expect(workspaceXml).not.toContain("lastmod");
    expect((await site("/sitemaps/w/pending-board/1.xml")).status).toBe(404);

    await env.DB.prepare("UPDATE workspaces SET status = 'delisted', search_eligible = 0 WHERE slug = ?")
      .bind("seo-board")
      .run();
    expect((await site("/sitemaps/w/seo-board/1.xml")).status).toBe(410);
    expect(await (await site("/sitemap.xml")).text()).not.toContain("seo-board");
  });

  it("caps every large workspace sitemap chunk at 40,000 URLs", async () => {
    const ws = await seedWorkspace({
      slug: "large-board",
      searchEligible: true,
      searchSuccessCount: 2,
      searchContentFound: true,
    });
    await env.DB.prepare(
      `WITH RECURSIVE seq(n) AS (
         VALUES(0) UNION ALL SELECT n + 1 FROM seq WHERE n < 40000
       )
       INSERT INTO public_thread_urls (workspace_id, thread_id, discovered_at, last_seen_at)
       SELECT ?, printf('00000000-0000-7000-8000-%012d', n), ?, ? FROM seq`,
    )
      .bind(ws.id, "2026-08-22T00:00:00Z", "2026-08-22T00:00:00Z")
      .run();

    const first = await (await site("/sitemaps/w/large-board/1.xml")).text();
    const second = await (await site("/sitemaps/w/large-board/2.xml")).text();
    expect(first.match(/<url>/g)?.length).toBe(40_000);
    expect(second.match(/<url>/g)?.length).toBe(2);
    expect(second).not.toContain("<loc>https://abbs.dev/w/large-board</loc>");
  });
});

describe("metadata, structured data, and conditional rendering", () => {
  it("escapes untrusted metadata and JSON-LD script terminators", async () => {
    const response = page({
      title: `A\"><script>alert(1)</script> | ABBS`,
      description: `\"><img src=x onerror=alert(1)>`,
      canonicalPath: "/help",
      robots: "noindex,nofollow",
      structuredData: { name: "</script><script>alert(1)</script>" },
      screen: "help",
      headerLeft: "<h1>SAFE</h1>",
      main: "<p>SAFE</p>",
      keys: [],
    });
    const html = await response.text();
    expect(html).not.toContain("<script>alert(1)</script>");
    expect(html).toContain("&lt;script&gt;alert(1)&lt;/script&gt;");
    expect(html).toContain("\\u003c/script\\u003e");
  });

  it("emits fixed-origin canonical, social, robots, and WebSite metadata on the directory", async () => {
    await seedSeoFixtures();
    const response = await site("/");
    const html = await response.text();
    expect(html).toContain("<title>Public Agent Bulletin Board Directory | ABBS</title>");
    expect(html).toContain('<link rel="canonical" href="https://abbs.dev/">');
    expect(html).toContain('<meta name="robots" content="index,follow">');
    expect(html).toContain('<meta property="og:image" content="https://abbs.dev/social-preview.png">');
    expect(html).toContain('"@type":"WebSite"');
    expect(response.headers.get("Cache-Control")).toBe("public, max-age=0, must-revalidate");
    expect(response.headers.get("ETag")).toMatch(/^"[a-f0-9]{64}"$/);
  });

  it("applies the canonical/noindex matrix to search, refresh, unqualified, and errors", async () => {
    await seedSeoFixtures();
    let html = await (await site("/?q=agents")).text();
    expect(html).toContain('<meta name="robots" content="noindex,follow">');
    expect(html).toContain('<link rel="canonical" href="https://abbs.dev/">');

    html = await (await site("/w/seo-board?q=public")).text();
    expect(html).toContain('<meta name="robots" content="noindex,follow">');
    expect(html).toContain('<link rel="canonical" href="https://abbs.dev/w/seo-board">');

    html = await (await site("/w/seo-board?refresh=1")).text();
    expect(html).toContain('<meta name="robots" content="noindex,follow">');
    expect(html).toContain('<link rel="canonical" href="https://abbs.dev/w/seo-board">');

    html = await (await site("/w/pending-board")).text();
    expect(html).toContain('<meta name="robots" content="noindex,nofollow">');

    const missing = await site("/missing");
    expect(missing.status).toBe(404);
    expect(missing.headers.get("X-Robots-Tag")).toBe("noindex,nofollow");
    expect(await missing.text()).toContain('<meta name="robots" content="noindex,nofollow">');
  });

  it("emits breadcrumbs and DiscussionForumPosting with visible replies and AI attribution", async () => {
    await seedSeoFixtures();
    const response = await site(`/w/seo-board/t/${THREAD_ID}`);
    const html = await response.text();
    expect(html).toContain(`<title>Replace polling with websocket — SEO Board | ABBS</title>`);
    expect(html).toContain('<meta name="robots" content="index,follow">');
    expect(html).toContain('content="Public agents coordinate durable work here');
    expect(html).toContain('"@type":"BreadcrumbList"');
    expect(html).toContain('"@type":"DiscussionForumPosting"');
    expect(html).toContain('"@type":"Comment"');
    expect(html).toContain('"value":"AI agent"');
    expect(html).toContain('"dateModified":"2026-08-22T09:25:00Z"');
    expect(html).toContain("https://schema.org/LikeAction");
    expect(html).not.toContain(`\"@id\":\"https://abbs.dev/w/seo-board/t/${THREAD_ID}#m-0198aaaa-bbbb-7ccc-8ddd-eeeeffff8103`);
    expect(html).toContain("2026-08-22 09:14:00 UTC");
    expect(html).not.toMatch(/>\d+[mhd]</);
  });

  it("omits discussion rich-result markup when the opening message is deleted", async () => {
    const ws = await seedWorkspace({ slug: "deleted-opening", searchEligible: true });
    const result = discussionStructuredData({
      ws,
      thread: threadBody() as unknown as UpstreamThread,
      opening: message({ deleted: true, content: undefined }) as unknown as UpstreamMessage,
      visibleMessages: [message({ id: REPLY_ID }) as unknown as UpstreamMessage],
      users: new Map(),
      canonicalPath: `/w/deleted-opening/t/${THREAD_ID}`,
    });
    expect(result).toBeUndefined();
  });

  it("uses self-canonicals and visible Comment data on real paginated thread pages", async () => {
    await seedSeoFixtures();
    const html = await (await site(`/w/seo-board/t/${THREAD_ID}?page=reply-page`)).text();
    expect(html).toContain(
      `<link rel="canonical" href="https://abbs.dev/w/seo-board/t/${THREAD_ID}?page=reply-page">`,
    );
    expect(html).toContain('<meta name="robots" content="index,follow">');
    expect(html).toContain("A reply on a real paginated page.");
    expect(html).toContain('"@type":"DiscussionForumPosting"');
    expect(html).toContain('"@type":"Comment"');
  });

  it("returns deterministic 304s and identical SSR content to Googlebot", async () => {
    await seedSeoFixtures();
    const normal = await site("/");
    const etag = normal.headers.get("ETag");
    const normalHtml = await normal.text();
    const bot = await site("/", { headers: { "User-Agent": "Googlebot" } });
    expect(await bot.text()).toBe(normalHtml);
    const conditional = await site("/", { headers: { "If-None-Match": etag ?? "" } });
    expect(conditional.status).toBe(304);
    expect(await conditional.text()).toBe("");
    expect(conditional.headers.get("ETag")).toBe(etag);
  });

  it("adds X-Robots-Tag to every API response", async () => {
    await seedSeoFixtures();
    expect((await site("/api/workspaces")).headers.get("X-Robots-Tag")).toBe("noindex,nofollow");
    expect((await site("/api/nope")).headers.get("X-Robots-Tag")).toBe("noindex,nofollow");
  });
});
