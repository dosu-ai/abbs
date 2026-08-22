// The deliberately small same-origin JSON API (WEBSITE_PLAN.md "Read proxy
// and caching"). Read-only in Phase 2: POST /api/workspaces arrives with
// Phase 3 registration. Upstream pagination tokens pass through as opaque
// values and cursors are never combined across workspaces.

import { discover } from "./health";
import { jsonResponse, problemResponse } from "./problems";
import { getWorkspace, listWorkspaces } from "./registry";
import type { Env, RegistryWorkspace } from "./types";
import {
  fetchMessages,
  fetchTags,
  fetchThread,
  fetchThreads,
  fetchUser,
  isUnreachable,
  validatePageParams,
} from "./upstream";
import type { PageParams, UpstreamErr } from "./upstream";

const PAGE_CACHE = { "Cache-Control": "public, max-age=30" };
const DIRECTORY_CACHE = { "Cache-Control": "public, max-age=30" };

function workspaceJson(ws: RegistryWorkspace): Record<string, unknown> {
  return {
    id: ws.id,
    slug: ws.slug,
    name: ws.name,
    description: ws.description,
    base_url: ws.baseUrl,
    canonical_url: ws.canonicalUrl,
    api_version: ws.apiVersion,
    status: ws.status,
    submitted_at: ws.submittedAt,
    last_checked_at: ws.lastCheckedAt,
    last_success_at: ws.lastSuccessAt,
    last_error_code: ws.lastErrorCode,
  };
}

// upstreamProblem maps a bounded upstream failure to a typed local error —
// never a reflection of whatever the upstream returned.
export function upstreamProblem(err: UpstreamErr): Response {
  if (err.code === "not-found") {
    return problemResponse(404, "not-found", "no such resource on this workspace");
  }
  if (isUnreachable(err.code)) {
    return problemResponse(504, "upstream-unreachable", `workspace did not answer (${err.code})`);
  }
  if (err.code === "rate-limited") {
    const headers =
      err.retryAfterSeconds !== undefined
        ? { "Retry-After": String(Math.min(err.retryAfterSeconds, 3600)) }
        : undefined;
    return problemResponse(503, "upstream-rate-limited", "workspace rate limited the directory", headers);
  }
  return problemResponse(502, "upstream-degraded", `workspace answered unexpectedly (${err.code})`);
}

async function withWorkspace(
  env: Env,
  slug: string,
  fn: (ws: RegistryWorkspace) => Promise<Response>,
): Promise<Response> {
  const ws = await getWorkspace(env.DB, slug);
  if (ws === null) return problemResponse(404, "not-found", "no such workspace");
  return fn(ws);
}

function pageParamsFrom(url: URL): PageParams | null {
  return validatePageParams({
    page: url.searchParams.get("page"),
    limit: url.searchParams.get("limit"),
    tags: url.searchParams.getAll("tag"),
  });
}

export async function apiListWorkspaces(env: Env): Promise<Response> {
  const items = await listWorkspaces(env.DB);
  return jsonResponse(200, { items: items.map(workspaceJson) }, DIRECTORY_CACHE);
}

export async function apiGetWorkspace(
  env: Env,
  ctx: ExecutionContext,
  slug: string,
  refresh: boolean,
): Promise<Response> {
  return withWorkspace(env, slug, async (ws) => {
    const d = await discover(env, ctx, ws, refresh);
    return jsonResponse(
      200,
      {
        workspace: workspaceJson(ws),
        server: d.result.ok ? d.result.value : null,
        upstream: {
          state: d.state,
          ...(d.result.ok ? {} : { error_code: d.result.code }),
        },
      },
      DIRECTORY_CACHE,
    );
  });
}

export async function apiThreads(
  env: Env,
  slug: string,
  url: URL,
  refresh: boolean,
): Promise<Response> {
  return withWorkspace(env, slug, async (ws) => {
    const params = pageParamsFrom(url);
    if (params === null) return problemResponse(400, "validation", "invalid page, limit, or tag");
    const r = await fetchThreads(ws, params, refresh);
    return r.ok ? jsonResponse(200, r.value, PAGE_CACHE) : upstreamProblem(r);
  });
}

export async function apiThread(
  env: Env,
  slug: string,
  threadId: string,
  refresh: boolean,
): Promise<Response> {
  return withWorkspace(env, slug, async (ws) => {
    const r = await fetchThread(ws, threadId, refresh);
    return r.ok ? jsonResponse(200, r.value, PAGE_CACHE) : upstreamProblem(r);
  });
}

export async function apiMessages(
  env: Env,
  slug: string,
  threadId: string,
  url: URL,
  refresh: boolean,
): Promise<Response> {
  return withWorkspace(env, slug, async (ws) => {
    const params = pageParamsFrom(url);
    if (params === null) return problemResponse(400, "validation", "invalid page or limit");
    const r = await fetchMessages(ws, threadId, params, refresh);
    return r.ok ? jsonResponse(200, r.value, PAGE_CACHE) : upstreamProblem(r);
  });
}

export async function apiTags(
  env: Env,
  slug: string,
  url: URL,
  refresh: boolean,
): Promise<Response> {
  return withWorkspace(env, slug, async (ws) => {
    const params = pageParamsFrom(url);
    if (params === null) return problemResponse(400, "validation", "invalid page or limit");
    const r = await fetchTags(ws, params, refresh);
    return r.ok ? jsonResponse(200, r.value, PAGE_CACHE) : upstreamProblem(r);
  });
}

export async function apiUser(env: Env, slug: string, username: string): Promise<Response> {
  return withWorkspace(env, slug, async (ws) => {
    const r = await fetchUser(ws, username);
    return r.ok ? jsonResponse(200, r.value, PAGE_CACHE) : upstreamProblem(r);
  });
}
