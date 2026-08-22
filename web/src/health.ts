// Live workspace health: discovery through the short cache, plus the
// opportunistic registry write-back that keeps directory status labels
// honest until Phase 3's scheduled verifier exists.

import { recordCheck } from "./registry";
import { fetchDiscovery, isUnreachable } from "./upstream";
import type { UpstreamResult } from "./upstream";
import type { Env, RegistryWorkspace, UpstreamServerInfo } from "./types";

// The state a screen renders for a workspace's connection. Every state has a
// text label; color is a reinforcement, never the only signal.
export type LiveState = "online" | "degraded" | "unreachable";

export interface Discovery {
  result: UpstreamResult<UpstreamServerInfo>;
  state: LiveState;
}

export function liveState(result: UpstreamResult<UpstreamServerInfo>): LiveState {
  if (result.ok) {
    return result.value.workspace.visibility === "public" ? "online" : "degraded";
  }
  return isUnreachable(result.code) ? "unreachable" : "degraded";
}

// discover fetches /v1/server (cached 5m) and, when the observation is
// fresh, persists it after the response via waitUntil. The discovery cache
// bounds the write rate; the read path never blocks on D1 writes.
export async function discover(
  env: Env,
  ctx: ExecutionContext,
  ws: RegistryWorkspace,
  refresh = false,
): Promise<Discovery> {
  const result = await fetchDiscovery(ws, refresh);
  if (result.fresh) {
    const now = new Date().toISOString();
    if (result.ok) {
      const w = result.value.workspace;
      if (w.visibility === "public") {
        ctx.waitUntil(
          recordCheck(env.DB, ws.id, now, {
            ok: true,
            name: w.name,
            description: w.description ?? "",
            apiVersion: result.value.api_version,
            canonicalUrl: w.canonical_url ?? null,
          }),
        );
      } else {
        // Reachable and conformant but no longer public: the listing decays
        // to degraded; Phase 3's verifier owns actual delisting.
        ctx.waitUntil(
          recordCheck(env.DB, ws.id, now, { ok: false, errorCode: "not-public" }),
        );
      }
    } else {
      ctx.waitUntil(
        recordCheck(env.DB, ws.id, now, {
          ok: false,
          errorCode: result.code,
          unreachable: isUnreachable(result.code),
        }),
      );
    }
  }
  return { result, state: liveState(result) };
}

// Manual refresh ([R]) bypasses the short caches within a bounded rate:
// a small per-address token bucket; exhausted refreshes silently fall back
// to cached reads instead of erroring.
const REFRESH_BURST = 5;
const REFRESH_REFILL_MS = 10_000; // one refresh credit per 10s
const refreshBuckets = new Map<string, { tokens: number; at: number }>();

export function allowRefresh(addr: string, now = Date.now()): boolean {
  if (refreshBuckets.size > 4096) refreshBuckets.clear();
  const b = refreshBuckets.get(addr) ?? { tokens: REFRESH_BURST, at: now };
  const refilled = Math.floor((now - b.at) / REFRESH_REFILL_MS);
  const tokens = Math.min(REFRESH_BURST, b.tokens + refilled);
  const at = tokens === REFRESH_BURST ? now : b.at + refilled * REFRESH_REFILL_MS;
  if (tokens < 1) {
    refreshBuckets.set(addr, { tokens, at });
    return false;
  }
  refreshBuckets.set(addr, { tokens: tokens - 1, at });
  return true;
}
