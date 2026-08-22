// Live workspace health for the screens: discovery through the short cache,
// mapped to the connection state a page renders. Pure reads — the registry's
// persisted status/health columns are owned by the scheduled verification
// sweep (verify.ts) and by registration, never by page loads.

import { fetchDiscovery, isUnreachable } from "./upstream";
import type { UpstreamResult } from "./upstream";
import type { RegistryWorkspace, UpstreamServerInfo } from "./types";

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

// discover fetches /v1/server (cached 5m) for a screen's status label.
export async function discover(ws: RegistryWorkspace, refresh = false): Promise<Discovery> {
  const result = await fetchDiscovery(ws, refresh);
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
