// Entry Worker for the ABBS public directory website. Server-rendered
// screens and the small /api surface share one route table; anything else
// falls through to same-origin static assets. Every route is a stable,
// shareable URL — deep-linking and refresh need no client-side state.
//
// The directory has exactly one public mutation: workspace registration
// (POST /api/workspaces, and POST /add for the no-JS form flow). No ABBS
// write request exists anywhere — registration only reads the candidate's
// public surface. The scheduled handler runs Phase 3's verification sweep.

import {
  apiGetWorkspace,
  apiListWorkspaces,
  apiMessages,
  apiTags,
  apiThread,
  apiThreads,
  apiUser,
} from "./api";
import { allowRefresh } from "./health";
import { boardPage } from "./pages/board";
import { directoryPage } from "./pages/directory";
import { addPage, helpPage } from "./pages/static";
import { threadPage } from "./pages/thread";
import { notFoundPage } from "./pages/shared";
import { problemResponse } from "./problems";
import { apiRegisterWorkspace, formRegisterWorkspace } from "./register";
import { runVerificationSweep } from "./verify";
import type { Env } from "./types";

const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;

function apiPath(pathname: string): boolean {
  return pathname === "/api" || pathname.startsWith("/api/");
}

function allowHeader(pathname: string): string {
  return pathname === "/add" || pathname === "/api/workspaces" ? "GET, HEAD, POST" : "GET, HEAD";
}

async function handle(request: Request, env: Env): Promise<Response> {
  const url = new URL(request.url);

  // The one mutation: workspace registration, as JSON or as the form flow.
  if (request.method === "POST") {
    if (url.pathname === "/api/workspaces") return apiRegisterWorkspace(request, env);
    if (url.pathname === "/add") return formRegisterWorkspace(request, env);
    return problemResponse(405, "method-not-allowed", "no such POST endpoint", {
      Allow: allowHeader(url.pathname),
    });
  }

  const method = request.method === "HEAD" ? "GET" : request.method;
  if (method !== "GET") {
    return problemResponse(405, "method-not-allowed", "unsupported method", {
      Allow: allowHeader(url.pathname),
    });
  }

  // Manual refresh bypasses the short caches within a bounded per-address
  // rate; over the limit it silently degrades to cached reads.
  const wantRefresh = url.searchParams.get("refresh") === "1";
  const addr = request.headers.get("CF-Connecting-IP") ?? "unknown";
  const refresh = wantRefresh && allowRefresh(addr);

  // Percent-decoded, non-empty path segments. Trailing slashes redirect to
  // the canonical URL rather than 404ing.
  const rawSegs = url.pathname.split("/").slice(1);
  if (rawSegs.length > 1 && rawSegs[rawSegs.length - 1] === "") {
    const canonical = url.pathname.replace(/\/+$/, "") + url.search;
    return Response.redirect(new URL(canonical === "" ? "/" : canonical, url.origin).href, 308);
  }
  let segs: string[];
  try {
    segs = rawSegs.filter((s) => s !== "").map(decodeURIComponent);
  } catch {
    return apiPath(url.pathname)
      ? problemResponse(404, "not-found", "malformed path")
      : notFoundPage();
  }

  // HTML screens.
  if (segs.length === 0) return directoryPage(env, url, refresh);
  if (segs.length === 1 && segs[0] === "add") return addPage();
  if (segs.length === 1 && segs[0] === "help") return helpPage();
  if (segs[0] === "w" && segs.length >= 2 && SLUG_RE.test(segs[1])) {
    if (segs.length === 2) return boardPage(env, segs[1], url, refresh);
    if (segs.length === 4 && segs[2] === "t") {
      return threadPage(env, segs[1], segs[3], url, refresh);
    }
  }

  // JSON API.
  if (segs[0] === "api" && segs[1] === "workspaces") {
    if (segs.length === 2) return apiListWorkspaces(env);
    const slug = segs[2];
    if (!SLUG_RE.test(slug)) return problemResponse(404, "not-found", "no such workspace");
    if (segs.length === 3) return apiGetWorkspace(env, slug, refresh);
    if (segs.length === 4 && segs[3] === "threads") return apiThreads(env, slug, url, refresh);
    if (segs.length === 5 && segs[3] === "threads") return apiThread(env, slug, segs[4], refresh);
    if (segs.length === 6 && segs[3] === "threads" && segs[5] === "messages") {
      return apiMessages(env, slug, segs[4], url, refresh);
    }
    if (segs.length === 4 && segs[3] === "tags") return apiTags(env, slug, url, refresh);
    if (segs.length === 5 && segs[3] === "users") return apiUser(env, slug, segs[4]);
  }

  // Static assets (styles, script, fonts, favicon), then 404. In production
  // matching asset paths are usually served before the Worker runs; this
  // covers local dev and any pass-through configuration.
  if (env.ASSETS !== undefined) {
    const asset = await env.ASSETS.fetch(request);
    if (asset.status !== 404) return asset;
  }
  return apiPath(url.pathname)
    ? problemResponse(404, "not-found", "no such endpoint")
    : notFoundPage();
}

export default {
  async fetch(request: Request, env: Env, _ctx: ExecutionContext): Promise<Response> {
    try {
      const resp = await handle(request, env);
      if (request.method === "HEAD") {
        return new Response(null, resp);
      }
      return resp;
    } catch (e) {
      console.error("unhandled", e);
      return problemResponse(500, "internal", "internal error");
    }
  },

  // Cron re-verification (wrangler.jsonc triggers): repeats discovery for
  // every listed workspace, refreshes cached metadata, and delists on lost
  // directory_listing consent. Locally: wrangler dev --test-scheduled and
  // curl "http://127.0.0.1:8787/__scheduled?cron=*+*+*+*+*".
  async scheduled(_controller: ScheduledController, env: Env, _ctx: ExecutionContext): Promise<void> {
    const summary = await runVerificationSweep(env);
    console.log("verification sweep", JSON.stringify(summary));
  },
} satisfies ExportedHandler<Env>;
