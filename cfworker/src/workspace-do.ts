// WorkspaceDO — one Durable Object per workspace, the whole ABBS server:
// schema init, the /v1 router, the write middleware, and the long-poll
// waiter set. The DO execution model is exactly the reference server's core
// constraints: single-threaded per object (sequence order equals commit
// order), transactional SQLite storage, and output gates that hold responses
// until writes are durable (ack ⇒ survives crash, by construction).

import { DurableObject } from "cloudflare:workers";
import type { ReqCtx, ServerCfg, TestHooks, Waiter } from "./context";
import type { Env, Limits, ServerInfo } from "./types";
import { defaultLimits } from "./types";
import { parseWorkspaceConfig } from "./config";
import { ProblemError, problemResponse } from "./problems";
import { bearerToken, mintToken, sha256Hex } from "./auth";
import { RateLimiter } from "./ratelimit";
import { DEFAULT_LOOP_GUARD } from "./loopguard";
import { Router } from "./router";
import { Store, events, type EventFilter } from "./store/store";
import { userByTokenHash } from "./store/users";
import { parseSeq } from "./text";
import { writeWrapped } from "./middleware";
import { handleAdmin, seedBootstrapAdmin } from "./admin";
import { authenticate, runHandler } from "./handlers/helpers";
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
import { handleEvents, parseEventQuery } from "./handlers/events";

const MAX_BODY_BYTES = 1 << 20; // the Go server's http.MaxBytesReader cap
const MAX_WAITERS = 256; // parked long-poll cap; unreachable in conformance
const MAX_WEBSOCKETS = 256; // per-workspace hibernatable socket cap
const MAX_WEBSOCKET_ATTACHMENT_BYTES = 2 << 10;
const ATTACHMENT_TOO_LARGE_DETAIL =
  "tag filter too large for the websocket transport on this server; narrow the tag filter or use GET /v1/events";

interface WebSocketAttachment {
  user: string;
  tokenHash: string;
  cursor: number;
  filter: EventFilter;
}

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
  private anonymousLimiter = new RateLimiter(60, 1);
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
    this.store = new Store(ctx.storage, () => this.notifyEventTransports());

    const workspace = parseWorkspaceConfig(env);
    const authMode = workspace.authMode;
    this.cfg = { authMode, visibility: workspace.visibility, loopGuard: DEFAULT_LOOP_GUARD };
    this.limits = defaultLimits();
    this.info = {
      api_version: "v1",
      workspace: {
        name: workspace.name,
        ...(workspace.description !== undefined ? { description: workspace.description } : {}),
        visibility: workspace.visibility,
        ...(workspace.canonicalUrl !== undefined ? { canonical_url: workspace.canonicalUrl } : {}),
        directory_listing: workspace.directoryListing,
      },
      auth_modes: [authMode],
      capabilities: ["websocket"],
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
    r.add("GET /v1/events/ws", false, (c) => this.handleEventsWebSocket(c));

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

  private notifyEventTransports(): void {
    this.notifyWaiters();
    this.deliverWebSocketEvents();
  }

  private handleEventsWebSocket(c: ReqCtx): Response {
    const user = authenticate(c);
    const query = parseEventQuery(c);
    if (!headerContainsToken(c.request.headers, "Upgrade", "websocket")) {
      throw new ProblemError(400, "validation", "missing Upgrade: websocket header");
    }
    if (this.ctx.getWebSockets().length >= MAX_WEBSOCKETS) {
      return problemResponse(503, "internal", "too many websocket connections");
    }

    // Keep the attachment within the transport's deliberately conservative
    // 2 KiB budget. Use the largest possible cursor in the preflight so a
    // later cursor advance cannot push an accepted socket over the limit.
    const attachment: WebSocketAttachment = {
      user: user.username,
      tokenHash: c.tokenHash!, // authenticate proved the bearer hash is present
      cursor: query.after,
      filter: query.filter,
    };
    const sizedAttachment = { ...attachment, cursor: Number.MAX_SAFE_INTEGER };
    if (serializedSize(sizedAttachment) > MAX_WEBSOCKET_ATTACHMENT_BYTES) {
      throw new ProblemError(400, "validation", ATTACHMENT_TOO_LARGE_DETAIL);
    }

    const pair = new WebSocketPair();
    const client = pair[0];
    const server = pair[1];
    this.ctx.acceptWebSocket(server);

    // Everything from acceptance through catch-up and attachment persistence
    // is synchronous. No append can interleave between the query and socket
    // registration in this Durable Object.
    try {
      const advanced = this.sendPendingEvents(server, attachment);
      server.serializeAttachment(advanced);
    } catch {
      closeWebSocket(server, 1011, "event delivery failed");
    }
    return new Response(null, { status: 101, webSocket: client });
  }

  private deliverWebSocketEvents(): void {
    let sockets: WebSocket[];
    try {
      sockets = this.ctx.getWebSockets();
    } catch {
      return;
    }
    for (const socket of sockets) {
      try {
        const attachment = deserializeWebSocketAttachment(socket);
        const principal = userByTokenHash(this.store, attachment.tokenHash);
        if (principal === null || principal.username !== attachment.user || principal.deactivated) {
          closeWebSocket(socket, 1008, "credentials revoked or user deactivated");
          continue;
        }
        const advanced = this.sendPendingEvents(socket, attachment);
        socket.serializeAttachment(advanced);
      } catch {
        closeWebSocket(socket, 1011, "event delivery failed");
      }
    }
  }

  private sendPendingEvents(socket: WebSocket, attachment: WebSocketAttachment): WebSocketAttachment {
    let cursor = attachment.cursor;
    for (;;) {
      const batch = events(this.store, attachment.user, cursor, this.limits.events_max_batch, attachment.filter);
      for (const event of batch.events) socket.send(JSON.stringify(event));
      if (batch.events.length === 0) break;

      const advanced = parseSeq(batch.cursor);
      if (advanced === null || advanced <= cursor) throw new Error("invalid event cursor");
      cursor = advanced;
      if (batch.events.length < this.limits.events_max_batch) break;
    }
    return { ...attachment, cursor };
  }

  // ABBS currently defines no client-to-server application frames. Ignore
  // them so the protocol retains additive headroom without waking any
  // application-level response path.
  webSocketMessage(_socket: WebSocket, _message: string | ArrayBuffer): void {}

  // Calling close remains safe with the runtime's automatic close replies
  // (enabled by our current compatibility date). Reserved synthetic codes
  // (notably 1006 for an abrupt disconnect) cannot appear in a close frame.
  webSocketClose(socket: WebSocket, code: number, reason: string, _wasClean: boolean): void {
    if (code === 1005 || code === 1006 || code === 1015) return;
    closeWebSocket(socket, code, reason);
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
        allowAnonymous: () => this.allowAnonymous(request),
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

  private allowAnonymous(request: Request): void {
    const observed = (request.headers.get("CF-Connecting-IP") ?? "").trim();
    const key = observed === "" ? "anonymous:fallback" : observed;
    const { ok, retryAfter } = this.anonymousLimiter.allow(key, Date.now());
    if (!ok) {
      throw new ProblemError(429, "rate-limited", "anonymous read rate limit", {
        "Retry-After": String(retryAfter),
      });
    }
  }
}

function headerContainsToken(headers: Headers, name: string, token: string): boolean {
  const value = headers.get(name);
  if (value === null) return false;
  return value.split(",").some((part) => part.trim().toLowerCase() === token.toLowerCase());
}

function serializedSize(value: unknown): number {
  return new TextEncoder().encode(JSON.stringify(value)).byteLength;
}

function deserializeWebSocketAttachment(socket: WebSocket): WebSocketAttachment {
  const value = socket.deserializeAttachment() as Partial<WebSocketAttachment> | null;
  if (
    value === null ||
    typeof value !== "object" ||
    typeof value.user !== "string" ||
    typeof value.tokenHash !== "string" ||
    typeof value.cursor !== "number" ||
    !Number.isSafeInteger(value.cursor) ||
    value.cursor < 0 ||
    value.filter === undefined ||
    typeof value.filter !== "object" ||
    typeof value.filter.mentions !== "boolean" ||
    typeof value.filter.dms !== "boolean" ||
    typeof value.filter.subscribedTags !== "boolean" ||
    !Array.isArray(value.filter.tags) ||
    value.filter.tags.some((tag) => typeof tag !== "string")
  ) {
    throw new Error("invalid websocket attachment");
  }
  return value as WebSocketAttachment;
}

function closeWebSocket(socket: WebSocket, code: number, reason: string): void {
  try {
    socket.close(code, reason);
  } catch {
    // A peer can disappear between getWebSockets() and close().
  }
}
