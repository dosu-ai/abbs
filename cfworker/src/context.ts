// The per-request context handlers receive: everything async (body read,
// token hashing) has already happened in the DO's fetch, so handlers and the
// store run strictly synchronous SQL — no await ever escapes the atomic
// section (input gates stay closed during it).

import type { Limits, ServerInfo } from "./types";
import type { Store } from "./store/store";
import type { LoopGuardConfig } from "./loopguard";

export interface ServerCfg {
  authMode: string;
  visibility: "private" | "public";
  loopGuard: LoopGuardConfig;
}

export interface Waiter {
  promise: Promise<void>;
  cancel: () => void;
}

// TestHooks instrument timing windows the black-box suite cannot reach
// deterministically (e.g. the events lost-wakeup window). Empty in
// production.
export interface TestHooks {
  // Fires after an events long-poll ran its query (empty) and before it
  // parks — the append-between-query-and-park window.
  afterEventsQuery?: () => void;
}

export interface ReqCtx {
  request: Request;
  url: URL;
  params: Record<string, string>;
  bodyText: string; // "" on read routes (body is only read for writes)
  tokenHash: string | null; // SHA-256 hex of the bearer token, when present
  // Pre-minted credential for the claim endpoint (minting hashes with
  // crypto.subtle, which is async and must not run inside a handler — write
  // handlers execute synchronously inside the idempotency transaction).
  mintedToken?: { token: string; tokenHash: string };
  store: Store;
  cfg: ServerCfg;
  limits: Limits;
  info: ServerInfo;
  // waitForEvent subscribes to the DO's waiter set — must be called before
  // querying so an append between query and park still wakes the poll.
  waitForEvent: () => Waiter;
  // Consumes the per-client anonymous GET budget or throws a 429 problem.
  allowAnonymous: () => void;
  hooks?: TestHooks;
}
