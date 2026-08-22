// WorkspaceDO — one Durable Object per workspace, the whole ABBS server:
// schema init, the /v1 router, the write middleware, and the long-poll
// waiter set. The DO execution model is exactly the reference server's core
// constraints: single-threaded per object (sequence order equals commit
// order), transactional SQLite storage, and output gates that hold responses
// until writes are durable (ack ⇒ survives crash, by construction).

import { DurableObject } from "cloudflare:workers";
import type { ReqCtx, ServerCfg, TestHooks, Waiter } from "./context";
import type { Env, Limits, ServerInfo } from "./types";
import { AUTH_API_KEY, AUTH_FIRST_CLAIM, defaultLimits } from "./types";
import { ProblemError, problemResponse } from "./problems";
import { bearerToken, mintToken, sha256Hex } from "./auth";
import { RateLimiter } from "./ratelimit";
import { DEFAULT_LOOP_GUARD } from "./loopguard";
import { Router } from "./router";
import { Store } from "./store/store";
import { writeWrapped } from "./middleware";
import { handleAdmin, seedBootstrapAdmin } from "./admin";
import { runHandler } from "./handlers/helpers";
import { handleGetServer } from "./handlers/server";
import { handleClaimUser, handleDeactivateUser, handleGetUser, handleListUsers } from "./handlers/users";
import { handleCreateThread, handleGetThread, handleListThreads } from "./handlers/threads";
import {
  handleDeleteMessage,
  handleEditMessage,
  handleGetMessage,
  handleListMessages,
  handlePostMessage,
} from "./handlers/messages";
import { handleAddReaction, handleListReactions, handleRemoveReaction } from "./handlers/reactions";
import {
  handleListTags,
  handleListTagSubscriptions,
  handleSubscribeTag,
  handleUnsubscribeTag,
  handleUpdateThreadTags,
} from "./handlers/tags";
import { handleGetReadCursor, handleInbox, handleSetReadCursor } from "./handlers/inbox";
import { handleEvents } from "./handlers/events";

const MAX_BODY_BYTES = 1 << 20; // the Go server's http.MaxBytesReader cap
const MAX_WAITERS = 256; // parked long-poll cap; unreachable in conformance

interface DrainedBody {
  text: string; // "" when there was no body or it was oversize
  oversize: boolean;
}

// drainBody consumes the entire request stream, keeping at most
// MAX_BODY_BYTES (anything longer is discarded while draining and flagged).
async function drainBody(request: Request): Promise<DrainedBody> {
  if (request.body === null) return { text: "", oversize: false };
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total <= MAX_BODY_BYTES) chunks.push(value);
  }
  if (total > MAX_BODY_BYTES) return { text: "", oversize: true };
  const buf = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    buf.set(c, off);
    off += c.byteLength;
  }
  return { text: new TextDecoder().decode(buf), oversize: false };
}

export class WorkspaceDO extends DurableObject<Env> {
  readonly store: Store;
  // Instrumentation seam for the unit suite (events lost-wakeup); empty in
  // production.
  readonly testHooks: TestHooks = {};
  private router = new Router();
  private limiter = new RateLimiter(60, 1); // burst 60, 1 token/s — the Go defaults
  private cfg: ServerCfg;
  private limits: Limits;
  private info: ServerInfo;

  // In-memory state (waiters, rate buckets, idempotency locks) is lost on DO
  // eviction — the same property as a Go server restart; accepted. The loop
  // guard is DB-backed and unaffected.
  private waiters = new Set<() => void>();
  private idemLocks = new Map<string, Promise<void>>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.store = new Store(ctx.storage, () => this.notifyWaiters());

    // Only the exact mode strings are accepted; anything else fails init
    // rather than silently falling back to first-claim — a typo'd production
    // AUTH_MODE must not enable unauthenticated identity claiming (parity
    // with the Go entrypoint, which rejects unsupported modes).
    const rawMode = env.AUTH_MODE ?? "";
    if (rawMode !== "" && rawMode !== AUTH_API_KEY && rawMode !== AUTH_FIRST_CLAIM) {
      throw new Error(`unsupported AUTH_MODE "${rawMode}" (want "${AUTH_FIRST_CLAIM}" or "${AUTH_API_KEY}")`);
    }
    const authMode = rawMode === AUTH_API_KEY ? AUTH_API_KEY : AUTH_FIRST_CLAIM;
    this.cfg = { authMode, loopGuard: DEFAULT_LOOP_GUARD };
    this.limits = defaultLimits();
    const description = env.WORKSPACE_DESCRIPTION ?? "";
    this.info = {
      api_version: "v1",
      workspace: {
        name: env.WORKSPACE_NAME || "abbs",
        ...(description !== "" ? { description } : {}),
      },
      auth_modes: [authMode],
      limits: this.limits,
    };

    // The route patterns are the idempotency scope keys — they must match
    // the Go server's mux patterns string-for-string.
    const r = this.router;
    r.add("GET /v1/server", false, handleGetServer);
    r.add("POST /v1/users", true, handleClaimUser);
    r.add("GET /v1/users", false, handleListUsers);
    r.add("GET /v1/users/{username}", false, handleGetUser);
    r.add("POST /v1/users/{username}/deactivate", true, handleDeactivateUser);
    r.add("POST /v1/threads", true, handleCreateThread);
    r.add("GET /v1/threads", false, handleListThreads);
    r.add("GET /v1/threads/{thread_id}", false, handleGetThread);
    r.add("PATCH /v1/threads/{thread_id}", true, handleUpdateThreadTags);
    r.add("GET /v1/threads/{thread_id}/messages", false, handleListMessages);
    r.add("POST /v1/threads/{thread_id}/messages", true, handlePostMessage);
    r.add("GET /v1/threads/{thread_id}/read-cursor", false, handleGetReadCursor);
    r.add("PUT /v1/threads/{thread_id}/read-cursor", true, handleSetReadCursor);
    r.add("GET /v1/messages/{message_id}", false, handleGetMessage);
    r.add("PATCH /v1/messages/{message_id}", true, handleEditMessage);
    r.add("DELETE /v1/messages/{message_id}", true, handleDeleteMessage);
    r.add("GET /v1/messages/{message_id}/reactions", false, handleListReactions);
    r.add("PUT /v1/messages/{message_id}/reactions/{emoji}", true, handleAddReaction);
    r.add("DELETE /v1/messages/{message_id}/reactions/{emoji}", true, handleRemoveReaction);
    r.add("GET /v1/tags", false, handleListTags);
    r.add("GET /v1/tag-subscriptions", false, handleListTagSubscriptions);
    r.add("PUT /v1/tag-subscriptions/{tag}", true, handleSubscribeTag);
    r.add("DELETE /v1/tag-subscriptions/{tag}", true, handleUnsubscribeTag);
    r.add("GET /v1/inbox", false, handleInbox);
    r.add("GET /v1/events", false, handleEvents);

    ctx.blockConcurrencyWhile(async () => {
      this.store.initSchema();
      await seedBootstrapAdmin(this.store, env);
    });
  }

  waiterCount(): number {
    return this.waiters.size;
  }

  private notifyWaiters(): void {
    const resolvers = [...this.waiters];
    this.waiters.clear();
    for (const resolve of resolvers) resolve();
  }

  private waitForEvent(): Waiter {
    if (this.waiters.size >= MAX_WAITERS) {
      // Degrade to plain polling under absurd load rather than growing the
      // set without bound.
      return { promise: new Promise((resolve) => setTimeout(resolve, 1000)), cancel: () => {} };
    }
    let resolve!: () => void;
    const promise = new Promise<void>((r) => (resolve = r));
    this.waiters.add(resolve);
    return { promise, cancel: () => this.waiters.delete(resolve) };
  }

  // One in-flight execution per idempotency key (see middleware.ts).
  private async withIdemLock<T>(key: string, f: () => Promise<T>): Promise<T> {
    while (this.idemLocks.has(key)) {
      await this.idemLocks.get(key);
    }
    let release!: () => void;
    this.idemLocks.set(key, new Promise<void>((r) => (release = r)));
    try {
      return await f();
    } finally {
      this.idemLocks.delete(key);
      release();
    }
  }

  async fetch(request: Request): Promise<Response> {
    // Drain the body before anything else: workerd errors if a forwarded
    // request stream is still pending when the response is sent, so every
    // body is consumed — even on 404s and read routes that ignore it. Bytes
    // past the cap are discarded while draining, never buffered.
    let body: DrainedBody;
    try {
      body = await drainBody(request);
    } catch {
      return problemResponse(400, "validation", "cannot read request body");
    }
    return this.route(request, body);
  }

  private async route(request: Request, body: DrainedBody): Promise<Response> {
    try {
      const url = new URL(request.url);

      // The operator plane sits outside /v1 and outside the spec, exactly
      // like the Go admin CLI.
      if (url.pathname === "/admin" || url.pathname.startsWith("/admin/")) {
        return await handleAdmin(this.store, this.env, request, url, body.text);
      }

      const m = this.router.match(request.method, url.pathname);
      if (m === null) {
        // No (method, path) match is always a 404 problem, never a 405 —
        // parity with the reference server's mux wrapper.
        return problemResponse(404, "not-found", "no such endpoint");
      }

      // All async work (body read, token hashing) happens strictly before
      // any handler runs; handlers and the store are then fully synchronous,
      // so no await can escape an atomic section.
      let bodyText = "";
      if (m.entry.write) {
        if (body.oversize) {
          return problemResponse(400, "validation", "cannot read request body");
        }
        bodyText = body.text;
      }
      const bearer = bearerToken(request);
      const tokenHash = bearer !== null ? await sha256Hex(bearer) : null;
      // The claim handler issues a credential; mint it here so the handler
      // itself stays synchronous (crypto.subtle is async, and write handlers
      // run inside the idempotency transaction).
      const mintedToken = m.entry.pattern === "POST /v1/users" ? await mintToken() : undefined;

      const c: ReqCtx = {
        request,
        url,
        params: m.params,
        bodyText,
        tokenHash,
        ...(mintedToken !== undefined ? { mintedToken } : {}),
        store: this.store,
        cfg: this.cfg,
        limits: this.limits,
        info: this.info,
        waitForEvent: () => this.waitForEvent(),
        hooks: this.testHooks,
      };

      if (m.entry.write) {
        return await writeWrapped(
          { limiter: this.limiter, withIdemLock: (k, f) => this.withIdemLock(k, f) },
          c,
          m.entry.pattern,
          m.entry.handler,
        );
      }
      return await runHandler(m.entry.handler, c);
    } catch (err) {
      if (err instanceof ProblemError) return err.response();
      return problemResponse(500, "internal", err instanceof Error ? err.message : String(err));
    }
  }
}
