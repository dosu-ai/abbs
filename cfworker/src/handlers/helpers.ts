// Shared handler plumbing: authentication, request decoding, validation
// helpers, and the store-error → problem mapping (port of the corresponding
// pieces of internal/server/server.go and handlers_full.go).

import type { ReqCtx } from "../context";
import type { Handler } from "../router";
import type { User } from "../types";
import { ProblemError, problemResponse } from "../problems";
import { StoreErr, UnknownParticipantErr } from "../store/store";
import { userByTokenHash } from "../store/users";
import { countCodePoints, parseIntStrict, parseSeq } from "../text";
import { normalizeEmoji } from "../emoji";

// authenticate resolves the bearer token to a principal, throwing the 401
// problem when it fails.
export function authenticate(c: ReqCtx): User {
  if (c.tokenHash === null) {
    throw new ProblemError(401, "unauthorized", "missing bearer token");
  }
  const user = userByTokenHash(c.store, c.tokenHash);
  if (user === null) {
    throw new ProblemError(401, "unauthorized", "unknown token");
  }
  if (user.deactivated) {
    throw new ProblemError(401, "unauthorized", "user is deactivated");
  }
  return user;
}

export function decodeJSON(c: ReqCtx): Record<string, unknown> {
  let v: unknown;
  try {
    v = JSON.parse(c.bodyText);
  } catch (err) {
    throw new ProblemError(400, "validation", "invalid JSON body: " + (err instanceof Error ? err.message : String(err)));
  }
  // The reference decoder tolerates a JSON null body (fields stay zero and
  // fail their own validation); other non-objects are decode errors there
  // and validation problems here either way.
  if (v === null) return {};
  if (typeof v !== "object" || Array.isArray(v)) {
    throw new ProblemError(400, "validation", "invalid JSON body: expected a JSON object");
  }
  return v as Record<string, unknown>;
}

// requireString rejects a present-but-mistyped field the way the reference
// server's typed JSON decoding does.
export function requireString(v: unknown, field: string): string {
  if (typeof v !== "string") {
    throw new ProblemError(400, "validation", `invalid JSON body: ${field} must be a string`);
  }
  return v;
}

export function optionalStringArray(v: unknown, field: string): string[] | null {
  if (v === undefined || v === null) return null;
  if (!Array.isArray(v) || v.some((x) => typeof x !== "string")) {
    throw new ProblemError(400, "validation", `invalid JSON body: ${field} must be an array of strings`);
  }
  return v as string[];
}

export function checkContent(c: ReqCtx, content: unknown): string {
  const s = requireString(content, "content");
  const n = countCodePoints(s);
  if (n === 0) {
    throw new ProblemError(400, "validation", "content must not be empty");
  }
  if (n > c.limits.message_max_chars) {
    // Rejected, never truncated — and a distinct code from validation.
    throw new ProblemError(
      422,
      "content-too-long",
      `content is ${n} code points; the limit is ${c.limits.message_max_chars}`,
    );
  }
  return s;
}

export function parseLimit(c: ReqCtx, def: number): number {
  const v = c.url.searchParams.get("limit");
  if (v === null || v === "") return def;
  const n = parseIntStrict(v);
  if (n === null || n < 1 || n > c.limits.page_max_limit) {
    throw new ProblemError(400, "validation", `limit must be 1..${c.limits.page_max_limit}`);
  }
  return n;
}

// parsePageAnchor turns an optional page token into a seq anchor (0 = none).
export function parsePageAnchor(c: ReqCtx): number {
  const p = c.url.searchParams.get("page");
  if (p === null || p === "") return 0;
  const n = parseSeq(p);
  if (n === null) {
    throw new ProblemError(400, "validation", "invalid page token");
  }
  return n;
}

// pathTag normalizes the {tag} path parameter.
export function pathTag(c: ReqCtx): string {
  const tag = c.params.tag.trim().toLowerCase();
  if (tag === "" || countCodePoints(tag) > c.limits.tag_max_chars) {
    throw new ProblemError(400, "validation", `tag must be 1..${c.limits.tag_max_chars} characters`);
  }
  return tag;
}

// pathEmoji validates and normalizes the {emoji} path parameter.
export function pathEmoji(c: ReqCtx): string {
  const key = normalizeEmoji(c.params.emoji);
  if (key === null) {
    throw new ProblemError(
      422,
      "invalid-emoji",
      "reactions must be a single Unicode emoji (one grapheme cluster with an emoji base)",
    );
  }
  return key;
}

// storeErrResponse is the port of mapStoreError: the problem for a store
// sentinel error.
export function storeErrResponse(err: StoreErr): Response {
  switch (err.code) {
    case "not-found":
      return problemResponse(404, "not-found", "no such resource");
    case "forbidden":
      return problemResponse(403, "forbidden", "not allowed");
    case "message-deleted":
      return problemResponse(409, "message-deleted", "the message is tombstoned");
    case "reaction-limit":
      return problemResponse(422, "reaction-limit", "at most 10 distinct emoji per user per message");
    default:
      return problemResponse(500, "internal", err.message);
  }
}

// runHandler executes a handler, turning thrown problems and store sentinel
// errors into problem+json responses — every handler exit is a Response.
export async function runHandler(handler: Handler, c: ReqCtx): Promise<Response> {
  try {
    return await handler(c);
  } catch (err) {
    if (err instanceof ProblemError) return err.response();
    if (err instanceof StoreErr) return storeErrResponse(err);
    if (err instanceof UnknownParticipantErr) return problemResponse(400, "validation", err.message);
    return problemResponse(500, "internal", err instanceof Error ? err.message : String(err));
  }
}

// runHandlerSync is runHandler for the write path, which executes handlers
// inside a transactionSync: write handlers are synchronous by construction
// (all async work happens in the DO's fetch, before any handler runs), and
// this enforces it — a Promise return is a programming error surfaced as a
// 500 rather than an await escaping the transaction.
export function runHandlerSync(handler: Handler, c: ReqCtx): Response {
  try {
    const resp = handler(c);
    if (resp instanceof Promise) {
      throw new Error("write handlers must be synchronous");
    }
    return resp;
  } catch (err) {
    if (err instanceof ProblemError) return err.response();
    if (err instanceof StoreErr) return storeErrResponse(err);
    if (err instanceof UnknownParticipantErr) return problemResponse(400, "validation", err.message);
    return problemResponse(500, "internal", err instanceof Error ? err.message : String(err));
  }
}

// notFound rethrows a store not-found with an endpoint-specific detail
// (e.g. "no such thread"), passing other errors through.
export function notFound(err: unknown, detail: string): never {
  if (err instanceof StoreErr && err.code === "not-found") {
    throw new ProblemError(404, "not-found", detail);
  }
  throw err;
}
