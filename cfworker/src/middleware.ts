// Port of internal/server/middleware.go — the shared write-path behaviors,
// in the same order as the Go write() wrapper after its anonymous-claim
// address gate (applied by workspace-do.ts): per-user rate limit, then
// Idempotency-Key semantics (per principal, per endpoint — the endpoint is
// the exact route-pattern string — ≥24h retention; identical replay returns
// the original response, headers included; body mismatch is a 409;
// purge-on-write).

import type { ReqCtx } from "./context";
import type { Handler } from "./router";
import { capturedBody, problemResponse } from "./problems";
import { sha256Hex } from "./auth";
import { idemGet, idemPut } from "./store/idempotency";
import { userByTokenHash } from "./store/users";
import { runHandler, runHandlerSync } from "./handlers/helpers";
import type { RateLimiter } from "./ratelimit";

const RETENTION_MS = 24 * 60 * 60 * 1000;

export interface WritePath {
  limiter: RateLimiter;
  // One in-flight execution per (principal, endpoint, key): concurrent
  // retries of the same key serialize here, so the loser replays the
  // winner's response instead of double-executing. Between get and put
  // everything is synchronous SQL inside one transaction, which input gates
  // make non-interleavable anyway — the lock is kept as refactor insurance.
  withIdemLock<T>(key: string, f: () => Promise<T>): Promise<T>;
}

// principalFor identifies who a write is charged to: the bearer principal,
// or — for the unauthenticated claim endpoint — the username being claimed.
// Empty means unidentifiable (the handler's own auth will reject it). A
// deactivated user is deliberately unidentifiable too: their credential must
// not replay cached responses (which may hold issued secrets) — the handler's
// authenticate() rejects them with the spec'd 401 instead.
function principalFor(c: ReqCtx): string {
  if (c.tokenHash !== null) {
    const user = userByTokenHash(c.store, c.tokenHash);
    if (user === null || user.deactivated) return "";
    return user.username;
  }
  if (c.request.method === "POST" && c.url.pathname === "/v1/users") {
    try {
      const req = JSON.parse(c.bodyText) as { username?: unknown };
      if (req && typeof req.username === "string" && req.username !== "") {
        return "claim:" + req.username;
      }
    } catch {
      // unparseable body: unidentifiable
    }
  }
  return "";
}

export async function writeWrapped(w: WritePath, c: ReqCtx, endpoint: string, handler: Handler): Promise<Response> {
  const principal = principalFor(c);
  if (principal !== "") {
    const { ok, retryAfter } = w.limiter.allow(principal, Date.now());
    if (!ok) {
      return problemResponse(429, "rate-limited", "per-user write rate limit", {
        "Retry-After": String(retryAfter),
      });
    }
  }

  const key = c.request.headers.get("Idempotency-Key") ?? "";
  if (key === "" || principal === "") {
    return runHandler(handler, c);
  }
  if (key.length > 128) {
    return problemResponse(400, "validation", "Idempotency-Key over 128 characters");
  }

  const reqHash = await sha256Hex(c.bodyText);
  const lockKey = `${principal}\x00${endpoint}\x00${key}`;
  // Everything from the cache lookup through the handler's mutation to the
  // idempotency record is one transactionSync: the mutation and its
  // remembered result commit together, so a reset can never leave a
  // committed mutation that a retry would execute a second time. Write
  // handlers are synchronous by construction (all async work — body read,
  // token hashing/minting — happened in the DO's fetch), so no await can
  // escape the transaction.
  return w.withIdemLock(lockKey, async () =>
    c.store.tx(() => {
      const cutoff = Date.now() - RETENTION_MS;
      const rec = idemGet(c.store, principal, endpoint, key, cutoff);
      if (rec !== null) {
        if (rec.requestHash !== reqHash) {
          return problemResponse(
            409,
            "idempotency-key-conflict",
            "Idempotency-Key was already used with a different request body",
          );
        }
        return new Response(rec.body === "" ? null : rec.body, {
          status: rec.status,
          headers: rec.headers,
        });
      }

      const resp = runHandlerSync(handler, c);
      if (resp.status < 500) {
        const body = capturedBody(resp);
        if (body === null) {
          // Every handler builds responses via problems.ts, which captures
          // the body string; anything else is a programming error.
          throw new Error("write handler produced a response with no captured body");
        }
        idemPut(
          c.store,
          principal,
          endpoint,
          key,
          { requestHash: reqHash, status: resp.status, headers: [...resp.headers], body },
          Date.now(),
          cutoff,
        );
      }
      return resp;
    }),
  );
}
