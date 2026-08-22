import { SITE_ORIGIN } from "./layout";
import type { Env, RegistryWorkspace } from "./types";
import { getWorkspaceBySlug } from "./registry";
import { problemResponse } from "./problems";

const CHUNK_SIZE = 40_000;
const CACHE_SECONDS = 15 * 60;

interface SitemapWorkspace extends RegistryWorkspace {
  threadCount: number;
  lastSeenAt: string | null;
}

function xmlEscape(value: string): string {
  return value.replace(/[&<>"']/g, (char) => {
    const values: Record<string, string> = {
      "&": "&amp;",
      "<": "&lt;",
      ">": "&gt;",
      '"': "&quot;",
      "'": "&apos;",
    };
    return values[char];
  });
}

function xmlResponse(body: string): Response {
  return new Response(body, {
    headers: {
      "Content-Type": "application/xml; charset=utf-8",
      "Cache-Control": `public, max-age=${CACHE_SECONDS}`,
      "X-Content-Type-Options": "nosniff",
    },
  });
}

async function cachedXml(key: string, build: () => string | Promise<string>): Promise<Response> {
  const request = new Request(`${SITE_ORIGIN}/__sitemap-cache/${encodeURIComponent(key)}`);
  try {
    const hit = await caches.default.match(request);
    if (hit !== undefined) return hit;
  } catch {
    // A cache miss and an unavailable cache have identical semantics.
  }
  const response = xmlResponse(await build());
  try {
    await caches.default.put(request, response.clone());
  } catch {
    // Sitemap generation remains available when the regional cache is not.
  }
  return response;
}

async function hashKey(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

async function eligibleWorkspaces(db: D1Database): Promise<SitemapWorkspace[]> {
  const rows = await db
    .prepare(
      `SELECT
         w.id, w.slug, w.base_url, w.canonical_url, w.name, w.description,
         w.api_version, w.status, w.submitted_at, w.last_checked_at,
         w.last_success_at, w.last_error_code, w.search_eligible,
         w.search_success_count, w.search_eligible_at, w.search_content_found,
         w.inventory_phase, w.inventory_cursor, w.inventory_anchor,
         w.inventory_completed_at, COUNT(p.thread_id) AS thread_count,
         MAX(p.last_seen_at) AS last_seen_at
       FROM workspaces w
       LEFT JOIN public_thread_urls p ON p.workspace_id = w.id
       WHERE w.search_eligible = 1 AND w.status != 'delisted'
       GROUP BY w.id
       ORDER BY w.slug ASC`,
    )
    .all<Record<string, unknown>>();

  return rows.results.map((row) => ({
    id: String(row.id),
    slug: String(row.slug),
    baseUrl: String(row.base_url),
    canonicalUrl: row.canonical_url === null ? null : String(row.canonical_url),
    name: String(row.name),
    description: String(row.description),
    apiVersion: row.api_version === null ? null : String(row.api_version),
    status: row.status as RegistryWorkspace["status"],
    submittedAt: String(row.submitted_at),
    lastCheckedAt: row.last_checked_at === null ? null : String(row.last_checked_at),
    lastSuccessAt: row.last_success_at === null ? null : String(row.last_success_at),
    lastErrorCode: row.last_error_code === null ? null : String(row.last_error_code),
    searchEligible: Number(row.search_eligible) === 1,
    searchSuccessCount: Number(row.search_success_count),
    searchEligibleAt: row.search_eligible_at === null ? null : String(row.search_eligible_at),
    searchContentFound: Number(row.search_content_found) === 1,
    inventoryPhase: row.inventory_phase as RegistryWorkspace["inventoryPhase"],
    inventoryCursor: row.inventory_cursor === null ? null : String(row.inventory_cursor),
    inventoryAnchor: row.inventory_anchor === null ? null : String(row.inventory_anchor),
    inventoryCompletedAt:
      row.inventory_completed_at === null ? null : String(row.inventory_completed_at),
    threadCount: Number(row.thread_count),
    lastSeenAt: row.last_seen_at === null ? null : String(row.last_seen_at),
  }));
}

export async function sitemapIndex(env: Env): Promise<Response> {
  // Query eligibility before every cache lookup. The cache key is a compact
  // inventory/eligibility fingerprint, so delisting changes it immediately.
  const workspaces = await eligibleWorkspaces(env.DB);
  const fingerprint = workspaces
    .map((ws) => `${ws.id}:${ws.threadCount}:${ws.lastSeenAt ?? ""}:${ws.searchEligibleAt ?? ""}`)
    .join("|");
  return cachedXml(`index:${await hashKey(fingerprint)}`, () => {
    const locations = [`${SITE_ORIGIN}/sitemaps/site.xml`];
    for (const ws of workspaces) {
      const chunks = Math.max(1, Math.ceil((ws.threadCount + 1) / CHUNK_SIZE));
      for (let chunk = 1; chunk <= chunks; chunk++) {
        locations.push(`${SITE_ORIGIN}/sitemaps/w/${encodeURIComponent(ws.slug)}/${chunk}.xml`);
      }
    }
    return `<?xml version="1.0" encoding="UTF-8"?>\n<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${locations
      .map((location) => `  <sitemap><loc>${xmlEscape(location)}</loc></sitemap>`)
      .join("\n")}\n</sitemapindex>\n`;
  });
}

export async function siteSitemap(): Promise<Response> {
  return cachedXml("site:v1", () => {
    const locations = [`${SITE_ORIGIN}/`, `${SITE_ORIGIN}/help`];
    return `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${locations
      .map((location) => `  <url><loc>${xmlEscape(location)}</loc></url>`)
      .join("\n")}\n</urlset>\n`;
  });
}

export async function workspaceSitemap(
  env: Env,
  slug: string,
  chunk: number,
): Promise<Response> {
  // The gate precedes the cache lookup. A delist/suspension therefore takes
  // effect even when the prior XML remains in a regional cache.
  const ws = await getWorkspaceBySlug(env.DB, slug);
  if (ws === null) return problemResponse(404, "not-found", "no such sitemap");
  if (ws.status === "delisted") return problemResponse(410, "delisted", "workspace delisted");
  if (!ws.searchEligible) return problemResponse(404, "not-found", "no such sitemap");

  const stats = await env.DB.prepare(
    `SELECT COUNT(*) AS count, MAX(last_seen_at) AS last_seen_at
     FROM public_thread_urls WHERE workspace_id = ?`,
  )
    .bind(ws.id)
    .first<{ count: number; last_seen_at: string | null }>();
  const count = Number(stats?.count ?? 0);
  const chunks = Math.max(1, Math.ceil((count + 1) / CHUNK_SIZE));
  if (!Number.isInteger(chunk) || chunk < 1 || chunk > chunks) {
    return problemResponse(404, "not-found", "no such sitemap chunk");
  }

  const firstThreadOffset = chunk === 1 ? 0 : CHUNK_SIZE - 1 + (chunk - 2) * CHUNK_SIZE;
  const threadLimit = chunk === 1 ? CHUNK_SIZE - 1 : CHUNK_SIZE;
  const key = `workspace:${ws.id}:${chunk}:${count}:${stats?.last_seen_at ?? ""}`;
  return cachedXml(key, async () => {
    const rows = await env.DB
      .prepare(
        `SELECT thread_id FROM public_thread_urls
         WHERE workspace_id = ? ORDER BY thread_id ASC LIMIT ? OFFSET ?`,
      )
      .bind(ws.id, threadLimit, firstThreadOffset)
      .all<{ thread_id: string }>();
    const base = `${SITE_ORIGIN}/w/${encodeURIComponent(ws.slug)}`;
    const locations = [
      ...(chunk === 1 ? [base] : []),
      ...rows.results.map((row) => `${base}/t/${encodeURIComponent(row.thread_id)}`),
    ];
    return `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${locations
      .map((location) => `  <url><loc>${xmlEscape(location)}</loc></url>`)
      .join("\n")}\n</urlset>\n`;
  });
}
