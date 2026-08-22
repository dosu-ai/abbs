// Hand-rolled router — load-bearing, not taste (cfworker/PLAN.md):
// (a) idempotency scope keys are the exact route-pattern strings
//     ("POST /v1/threads/{thread_id}/messages", per the Go middleware), so
//     the router must expose matched patterns;
// (b) the Go mux returns a 404 problem for method mismatches, never 405 —
//     no (method, path) match is always 404 not-found.

import type { ReqCtx } from "./context";

export type Handler = (c: ReqCtx) => Promise<Response> | Response;

export interface RouteEntry {
  pattern: string; // "METHOD /path/{param}" — the idempotency scope key
  write: boolean; // wrapped with the write middleware (rate limit + idempotency)
  handler: Handler;
}

export interface Matched {
  entry: RouteEntry;
  params: Record<string, string>;
}

interface CompiledRoute {
  method: string;
  segments: string[]; // "{name}" segments capture; others are literal
  entry: RouteEntry;
}

export class Router {
  private routes: CompiledRoute[] = [];

  add(pattern: string, write: boolean, handler: Handler): void {
    const sp = pattern.indexOf(" ");
    const method = pattern.slice(0, sp);
    const path = pattern.slice(sp + 1);
    const segments = path.split("/").filter((s) => s !== "");
    this.routes.push({ method, segments, entry: { pattern, write, handler } });
  }

  // match matches method + raw (still percent-encoded) pathname. Wildcard
  // segments capture exactly one non-empty segment, percent-decoded.
  match(method: string, pathname: string): Matched | null {
    const raw = pathname.split("/");
    // A trailing slash produces a trailing empty segment and must not match
    // (the Go mux is exact); interior empty segments must not match either.
    if (raw[raw.length - 1] === "") return null;
    const parts = raw.filter((s) => s !== "");
    if (parts.length !== raw.length - 1) return null;

    for (const r of this.routes) {
      if (r.method !== method || r.segments.length !== parts.length) continue;
      const params: Record<string, string> = {};
      let ok = true;
      for (let i = 0; i < parts.length; i++) {
        const seg = r.segments[i];
        let decoded: string;
        try {
          decoded = decodeURIComponent(parts[i]);
        } catch {
          ok = false;
          break;
        }
        if (seg.startsWith("{") && seg.endsWith("}")) {
          params[seg.slice(1, -1)] = decoded;
        } else if (seg !== decoded) {
          ok = false;
          break;
        }
      }
      if (ok) return { entry: r.entry, params };
    }
    return null;
  }
}
