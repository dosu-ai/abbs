// Port of internal/server/middleware.go — the two write-path behaviors, in
// the same order as the Go write() wrapper: per-user rate limit, then
// Idempotency-Key semantics (per principal, per endpoint — the endpoint is
// the exact route-pattern string — ≥24h retention; identical replay returns
// the original response; body mismatch is a 409; purge-on-write).

import type { ReqCtx } from "./context";
import type { Handler } from "./router";
import { problemResponse } from "./problems";
import { sha256Hex } from "./auth";
import { idemGet, idemPut } from "./store/idempotency";
import { userByTokenHash } from "./store/users";
import { runHandler } from "./handlers/helpers";
import type { RateLimiter } from "./ratelimit";

const RETENTION_MS = 24 * 60 * 60 * 1000;

export interface WritePath {
  limiter: RateLimiter;
  // One in-flight execution per (principal, endpoint, key): concurrent
  // retries of the same key serialize here, so the loser replays the
  // winner's response instead of double-executing. Between get and put
  // everything is synchronous SQL, which input gates make non-interleavable
  // anyway — the lock is kept as refactor insurance.
  withIdemLock<T>(key: string, f: () => Promise<T>): Promise<T>;
}

// principalFor identifies who a write is charged to: the bearer principal,
// or — for the unauthenticated claim endpoint — the username being claimed.
// Empty means unidentifiable (the handler's own auth will reject it).
function principalFor(c: ReqCtx): string {
  if (c.tokenHash !== null) {
    return userByTokenHash(c.store, c.tokenHash)?.username ?? "";
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
  return w.withIdemLock(lockKey, async () => {
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
      return replay(rec.status, rec.contentType, rec.body);
    }

    const resp = await runHandler(handler, c);
    const status = resp.status;
    const contentType = resp.headers.get("Content-Type") ?? "";
    const body = await resp.text();
    if (status < 500) {
      idemPut(
        c.store,
        principal,
        endpoint,
        key,
        { requestHash: reqHash, status, contentType, body },
        Date.now(),
        cutoff,
      );
    }
    // The original response body was consumed above; rebuild it byte-for-byte.
    const headers = new Headers(resp.headers);
    return new Response(body === "" ? null : body, { status, headers });
  });
}

function replay(status: number, contentType: string, body: string): Response {
  const headers = new Headers();
  if (contentType !== "") headers.set("Content-Type", contentType);
  return new Response(body === "" ? null : body, { status, headers });
}
