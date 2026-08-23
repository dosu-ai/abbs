import { env } from "cloudflare:test";
import type { RegistryWorkspace, WorkspaceStatus } from "../src/types";

let counter = 0;

// seedWorkspace inserts a registry row and returns it in RegistryWorkspace
// shape. Base URLs default to a unique mockable origin per call.
export async function seedWorkspace(
  over: Partial<{
    id: string;
    slug: string;
    baseUrl: string;
    name: string;
    description: string;
    status: WorkspaceStatus;
    searchEligible: boolean;
    searchSuccessCount: number;
    searchContentFound: boolean;
  }> = {},
): Promise<RegistryWorkspace> {
  counter++;
  const slug = over.slug ?? `ws-${counter}`;
  const ws: RegistryWorkspace = {
    id: over.id ?? `0198c0de-0000-7000-8000-${String(counter).padStart(12, "0")}`,
    slug,
    baseUrl: over.baseUrl ?? `https://${slug}.example`,
    canonicalUrl: null,
    name: over.name ?? slug,
    description: over.description ?? `test workspace ${slug}`,
    apiVersion: null,
    status: over.status ?? "active",
    submittedAt: "2026-08-22T00:00:00Z",
    lastCheckedAt: null,
    lastSuccessAt: null,
    lastErrorCode: null,
    searchEligible: over.searchEligible ?? false,
    searchSuccessCount: over.searchSuccessCount ?? 0,
    searchEligibleAt: over.searchEligible ? "2026-08-22T00:30:00Z" : null,
    searchContentFound: over.searchContentFound ?? false,
    inventoryPhase: "bootstrap",
    inventoryCursor: null,
    inventoryAnchor: null,
    inventoryCompletedAt: null,
  };
  await env.DB.prepare(
    `INSERT INTO workspaces
       (id, slug, base_url, name, description, status, submitted_at,
        search_eligible, search_success_count, search_eligible_at, search_content_found)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  )
    .bind(
      ws.id,
      ws.slug,
      ws.baseUrl,
      ws.name,
      ws.description,
      ws.status,
      ws.submittedAt,
      ws.searchEligible ? 1 : 0,
      ws.searchSuccessCount,
      ws.searchEligibleAt,
      ws.searchContentFound ? 1 : 0,
    )
    .run();
  return ws;
}

export function serverInfoBody(name: string, extra: Record<string, unknown> = {}): string {
  return JSON.stringify({
    api_version: "v1",
    workspace: {
      name,
      description: `${name} description`,
      visibility: "public",
      canonical_url: `https://${name}.example`,
      directory_listing: true,
      ...extra,
    },
    auth_modes: ["first-claim"],
    limits: {},
  });
}

export const JSON_HEADERS = { headers: { "Content-Type": "application/json" } };

export const THREAD_ID = "0198aaaa-bbbb-7ccc-8ddd-eeeeffff0001";

export function threadBody(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: THREAD_ID,
    kind: "public",
    title: "Replace polling with websocket",
    tags: ["api", "transport"],
    creator: "ada",
    created_at: "2026-08-22T09:14:00Z",
    created_seq: "188",
    last_activity_seq: "193",
    ...over,
  };
}

export function pageBody(items: unknown[], nextPage: string | null = null, asOf = "200"): string {
  return JSON.stringify({ items, next_page: nextPage, as_of: asOf });
}
