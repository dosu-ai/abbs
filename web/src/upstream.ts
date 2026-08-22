// The constrained read proxy (WEBSITE_PLAN.md "Read proxy and caching").
//
// Invariants, enforced here and nowhere weaker:
//   - Only already-registered workspaces are contacted, never an arbitrary
//     destination URL, and only over HTTPS (loopback excepted for local dev).
//   - Only the allowlisted anonymous ABBS GET surface is requested; every
//     path parameter and query value is validated before URL construction.
//   - No browser header is forwarded upstream; no upstream header leaks back.
//   - Time, size, and redirect limits apply to every request.
//   - Failures become bounded error codes, never reflected upstream bodies.
//   - Short in-memory response caches (discovery 5m, pages 30s, errors 5s)
//     absorb load; a bounded manual-refresh bypass revalidates on demand.

import type {
  RegistryWorkspace,
  UpstreamMessage,
  UpstreamPage,
  UpstreamPublicUser,
  UpstreamServerInfo,
  UpstreamTagInfo,
  UpstreamThread,
} from "./types";

export type UpstreamErrorCode =
  | "insecure-url"
  | "timeout"
  | "network"
  | "redirect"
  | "http-4xx"
  | "not-found"
  | "rate-limited"
  | "http-5xx"
  | "bad-content-type"
  | "too-large"
  | "bad-json"
  | "bad-schema"
  | "not-public";

export interface UpstreamOk<T> {
  ok: true;
  value: T;
  // True when this result came from a live upstream exchange rather than the
  // cache — the signal that a health observation is worth persisting.
  fresh: boolean;
}

export interface UpstreamErr {
  ok: false;
  code: UpstreamErrorCode;
  status?: number;
  retryAfterSeconds?: number;
  fresh: boolean;
}

export type UpstreamResult<T> = UpstreamOk<T> | UpstreamErr;

// A transport failure means unreachable; anything else (wrong protocol,
// wrong policy, upstream errors) is degraded.
export function isUnreachable(code: UpstreamErrorCode): boolean {
  return code === "timeout" || code === "network";
}

const FETCH_TIMEOUT_MS = 6_000;
const MAX_BODY_BYTES = 1_048_576; // 1 MiB
const DISCOVERY_TTL_MS = 300_000; // 5 minutes: directory/discovery metadata
const PAGE_TTL_MS = 30_000; // 30 seconds: threads, messages, tags (edits/tombstones)
const ERROR_TTL_MS = 5_000; // upstream errors: at most a few seconds
const CACHE_MAX_ENTRIES = 512;

// Validation for values that become upstream URL components. Anything that
// fails validation is rejected locally as not-found/validation — it never
// reaches the network.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const USERNAME_RE = /^[a-z0-9][a-z0-9._-]{0,31}$/;
const TAG_MAX_CHARS = 64;
const MAX_TAGS = 16;
const PAGE_TOKEN_MAX_CHARS = 256;

export function isValidThreadId(v: string): boolean {
  return UUID_RE.test(v);
}

export function isValidUsername(v: string): boolean {
  return USERNAME_RE.test(v);
}

export interface PageParams {
  page?: string;
  limit?: number;
  tags?: string[];
}

// validatePageParams normalizes untrusted query values into safe upstream
// parameters, or null when anything is out of contract.
export function validatePageParams(p: {
  page?: string | null;
  limit?: string | null;
  tags?: string[];
}): PageParams | null {
  const out: PageParams = {};
  if (p.page != null && p.page !== "") {
    if (p.page.length > PAGE_TOKEN_MAX_CHARS) return null;
    out.page = p.page;
  }
  if (p.limit != null && p.limit !== "") {
    if (!/^\d{1,3}$/.test(p.limit)) return null;
    const n = Number(p.limit);
    if (n < 1 || n > 100) return null;
    out.limit = n;
  }
  if (p.tags !== undefined && p.tags.length > 0) {
    if (p.tags.length > MAX_TAGS) return null;
    for (const t of p.tags) {
      if (t.length === 0 || t.length > TAG_MAX_CHARS) return null;
    }
    out.tags = p.tags;
  }
  return out;
}

// Outbound fetch seam. Tests substitute this (vitest-pool-workers no longer
// ships a cloudflare:test fetchMock); production always uses global fetch.
type FetchLike = (input: string, init: RequestInit) => Promise<Response>;
let fetchFn: FetchLike = (input, init) => fetch(input, init);

export function setUpstreamFetchForTests(f: FetchLike | null): void {
  fetchFn = f ?? ((input, init) => fetch(input, init));
}

interface CacheEntry {
  expiresAt: number;
  result: UpstreamResult<unknown>;
}

// Per-isolate response cache. Deliberately not the global Cache API: entries
// are tiny, TTLs are seconds-to-minutes, and an in-memory map is exactly as
// testable as the plan needs for Phase 2.
const cache = new Map<string, CacheEntry>();

// Test seam only.
export function clearUpstreamCache(): void {
  cache.clear();
}

function cacheGet(key: string, now: number): UpstreamResult<unknown> | null {
  const e = cache.get(key);
  if (e === undefined) return null;
  if (e.expiresAt <= now) {
    cache.delete(key);
    return null;
  }
  return e.result;
}

function cachePut(key: string, result: UpstreamResult<unknown>, ttlMs: number, now: number): void {
  if (cache.size >= CACHE_MAX_ENTRIES) {
    // Maps iterate in insertion order; dropping the oldest entry bounds
    // memory without bookkeeping.
    const oldest = cache.keys().next();
    if (!oldest.done) cache.delete(oldest.value);
  }
  cache.set(key, { expiresAt: now + ttlMs, result });
}

function parseRetryAfter(h: string | null): number | undefined {
  if (h === null || !/^\d{1,6}$/.test(h.trim())) return undefined;
  return Number(h.trim());
}

function isLoopbackHost(hostname: string): boolean {
  return hostname === "localhost" || hostname === "127.0.0.1" || hostname === "[::1]";
}

// readBodyCapped reads at most MAX_BODY_BYTES; anything larger aborts.
async function readBodyCapped(resp: Response): Promise<string | null> {
  const body = resp.body;
  if (body === null) return "";
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > MAX_BODY_BYTES) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const joined = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    joined.set(c, off);
    off += c.byteLength;
  }
  return new TextDecoder().decode(joined);
}

type Validator<T> = (v: unknown) => T | null;

async function upstreamGet<T>(
  ws: RegistryWorkspace,
  path: string,
  query: URLSearchParams,
  ttlMs: number,
  validate: Validator<T>,
  refresh: boolean,
): Promise<UpstreamResult<T>> {
  const qs = query.toString();
  const key = `${ws.id} ${path}${qs === "" ? "" : "?" + qs}`;
  const now = Date.now();
  if (!refresh) {
    const hit = cacheGet(key, now);
    if (hit !== null) return { ...hit, fresh: false } as UpstreamResult<T>;
  }

  const result = await liveGet(ws, path, qs, validate);
  cachePut(key, result, result.ok ? ttlMs : errorTtl(result), now);
  return result;
}

function errorTtl(err: UpstreamErr): number {
  // Honor upstream backoff (bounded), otherwise cache errors only briefly.
  if (err.code === "rate-limited" && err.retryAfterSeconds !== undefined) {
    return Math.min(err.retryAfterSeconds, 60) * 1000;
  }
  return ERROR_TTL_MS;
}

async function liveGet<T>(
  ws: RegistryWorkspace,
  path: string,
  qs: string,
  validate: Validator<T>,
): Promise<UpstreamResult<T>> {
  let base: URL;
  try {
    base = new URL(ws.baseUrl);
  } catch {
    return { ok: false, code: "insecure-url", fresh: true };
  }
  if (base.protocol !== "https:" && !isLoopbackHost(base.hostname)) {
    return { ok: false, code: "insecure-url", fresh: true };
  }

  const url = base.origin + path + (qs === "" ? "" : "?" + qs);
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), FETCH_TIMEOUT_MS);
  let resp: Response;
  try {
    // A fresh request with exactly these headers: the visitor's cookies,
    // authorization, and everything else never crosses this boundary.
    resp = await fetchFn(url, {
      method: "GET",
      redirect: "manual",
      signal: ctrl.signal,
      headers: {
        Accept: "application/json",
        "User-Agent": "abbs-directory (+https://abbs.dev)",
      },
    });
  } catch {
    return {
      ok: false,
      code: ctrl.signal.aborted ? "timeout" : "network",
      fresh: true,
    };
  } finally {
    clearTimeout(timer);
  }

  if (resp.status >= 300 && resp.status < 400) {
    await resp.body?.cancel();
    return { ok: false, code: "redirect", status: resp.status, fresh: true };
  }
  if (resp.status === 429) {
    await resp.body?.cancel();
    return {
      ok: false,
      code: "rate-limited",
      status: 429,
      retryAfterSeconds: parseRetryAfter(resp.headers.get("Retry-After")),
      fresh: true,
    };
  }
  if (resp.status >= 500) {
    await resp.body?.cancel();
    return { ok: false, code: "http-5xx", status: resp.status, fresh: true };
  }
  if (resp.status !== 200) {
    await resp.body?.cancel();
    return {
      ok: false,
      code: resp.status === 404 ? "not-found" : "http-4xx",
      status: resp.status,
      fresh: true,
    };
  }

  const ct = resp.headers.get("Content-Type") ?? "";
  if (!/^application\/(problem\+)?json\b/i.test(ct)) {
    await resp.body?.cancel();
    return { ok: false, code: "bad-content-type", fresh: true };
  }

  let text: string | null;
  try {
    text = await readBodyCapped(resp);
  } catch {
    return { ok: false, code: "network", fresh: true };
  }
  if (text === null) return { ok: false, code: "too-large", fresh: true };

  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { ok: false, code: "bad-json", fresh: true };
  }

  const value = validate(parsed);
  if (value === null) return { ok: false, code: "bad-schema", fresh: true };
  return { ok: true, value, fresh: true };
}

// --- shape validation -------------------------------------------------------
// Minimal structural checks: enough to guarantee the renderer and API layer
// only ever see protocol-shaped values. Content remains untrusted input and
// is escaped at render time regardless.

function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function str(v: unknown, max: number): string | null {
  return typeof v === "string" && v.length <= max ? v : null;
}

function validateServerInfo(v: unknown): UpstreamServerInfo | null {
  if (!isObj(v)) return null;
  const apiVersion = str(v.api_version, 32);
  if (apiVersion === null) return null;
  if (!isObj(v.workspace)) return null;
  const w = v.workspace;
  const name = str(w.name, 100);
  if (name === null || name.length === 0) return null;
  const visibility = w.visibility;
  if (visibility !== "public" && visibility !== "private") return null;
  const description = w.description === undefined ? undefined : (str(w.description, 1000) ?? undefined);
  const canonicalUrl = w.canonical_url === undefined ? undefined : (str(w.canonical_url, 512) ?? undefined);
  return {
    api_version: apiVersion,
    workspace: {
      name,
      ...(description !== undefined ? { description } : {}),
      visibility,
      ...(canonicalUrl !== undefined ? { canonical_url: canonicalUrl } : {}),
      directory_listing: w.directory_listing === true,
    },
  };
}

function validatePageOf<T>(itemValidate: Validator<T>): Validator<UpstreamPage<T>> {
  return (v: unknown) => {
    if (!isObj(v)) return null;
    if (!Array.isArray(v.items) || v.items.length > 100) return null;
    const asOf = str(v.as_of, 64);
    if (asOf === null) return null;
    const nextPage = v.next_page === null ? null : str(v.next_page, 256);
    if (v.next_page !== null && nextPage === null) return null;
    const items: T[] = [];
    for (const raw of v.items) {
      const item = itemValidate(raw);
      if (item === null) return null;
      items.push(item);
    }
    return { items, next_page: nextPage, as_of: asOf };
  };
}

function validateThread(v: unknown): UpstreamThread | null {
  if (!isObj(v)) return null;
  const id = str(v.id, 64);
  const kind = str(v.kind, 16);
  const title = str(v.title, 200);
  const creator = str(v.creator, 32);
  const createdAt = str(v.created_at, 64);
  const createdSeq = str(v.created_seq, 64);
  const lastActivitySeq = str(v.last_activity_seq, 64);
  if (
    id === null || !UUID_RE.test(id) || kind === null || title === null ||
    creator === null || createdAt === null || createdSeq === null ||
    lastActivitySeq === null || !Array.isArray(v.tags) || v.tags.length > 16
  ) {
    return null;
  }
  const tags: string[] = [];
  for (const t of v.tags) {
    const tag = str(t, TAG_MAX_CHARS);
    if (tag === null) return null;
    tags.push(tag);
  }
  return {
    id,
    kind,
    title,
    tags,
    creator,
    created_at: createdAt,
    created_seq: createdSeq,
    last_activity_seq: lastActivitySeq,
  };
}

function validateMessage(v: unknown): UpstreamMessage | null {
  if (!isObj(v)) return null;
  const id = str(v.id, 64);
  const threadId = str(v.thread_id, 64);
  const author = str(v.author, 32);
  const createdAt = str(v.created_at, 64);
  const seq = str(v.seq, 64);
  if (
    id === null || !UUID_RE.test(id) || threadId === null || author === null ||
    createdAt === null || seq === null || typeof v.deleted !== "boolean" ||
    !Array.isArray(v.reactions) || v.reactions.length > 64
  ) {
    return null;
  }
  const reactions: { emoji: string; count: number }[] = [];
  for (const r of v.reactions) {
    if (!isObj(r)) return null;
    const emoji = str(r.emoji, 32);
    if (emoji === null || typeof r.count !== "number" || r.count < 1) return null;
    reactions.push({ emoji, count: Math.floor(r.count) });
  }
  const out: UpstreamMessage = {
    id,
    thread_id: threadId,
    author,
    deleted: v.deleted,
    created_at: createdAt,
    seq,
    reactions,
  };
  if (!v.deleted) {
    // 8k code points is the protocol limit; the cap here is defensive.
    const content = str(v.content, 65536);
    if (content === null) return null;
    out.content = content;
  }
  if (typeof v.edited_at === "string") out.edited_at = v.edited_at.slice(0, 64);
  if (typeof v.deleted_at === "string") out.deleted_at = v.deleted_at.slice(0, 64);
  if (typeof v.deleted_by === "string") out.deleted_by = v.deleted_by.slice(0, 32);
  return out;
}

function validateTagInfo(v: unknown): UpstreamTagInfo | null {
  if (!isObj(v)) return null;
  const name = str(v.name, TAG_MAX_CHARS);
  if (name === null || typeof v.thread_count !== "number" || v.thread_count < 0) return null;
  return { name, thread_count: Math.floor(v.thread_count) };
}

function validatePublicUser(v: unknown): UpstreamPublicUser | null {
  if (!isObj(v)) return null;
  const username = str(v.username, 32);
  if (username === null) return null;
  if (v.kind !== "human" && v.kind !== "agent") return null;
  const displayName = v.display_name === undefined ? undefined : (str(v.display_name, 100) ?? undefined);
  return {
    username,
    kind: v.kind,
    ...(displayName !== undefined ? { display_name: displayName } : {}),
  };
}

// --- the allowlisted anonymous read surface ---------------------------------

function pageQuery(p: PageParams): URLSearchParams {
  const q = new URLSearchParams();
  if (p.tags !== undefined) for (const t of p.tags) q.append("tag", t);
  if (p.page !== undefined) q.set("page", p.page);
  if (p.limit !== undefined) q.set("limit", String(p.limit));
  return q;
}

export function fetchDiscovery(
  ws: RegistryWorkspace,
  refresh = false,
): Promise<UpstreamResult<UpstreamServerInfo>> {
  return upstreamGet(ws, "/v1/server", new URLSearchParams(), DISCOVERY_TTL_MS, validateServerInfo, refresh);
}

export function fetchThreads(
  ws: RegistryWorkspace,
  params: PageParams,
  refresh = false,
): Promise<UpstreamResult<UpstreamPage<UpstreamThread>>> {
  return upstreamGet(ws, "/v1/threads", pageQuery(params), PAGE_TTL_MS, validatePageOf(validateThread), refresh);
}

export function fetchThread(
  ws: RegistryWorkspace,
  threadId: string,
  refresh = false,
): Promise<UpstreamResult<UpstreamThread>> {
  if (!isValidThreadId(threadId)) {
    return Promise.resolve({ ok: false, code: "not-found", status: 404, fresh: false });
  }
  return upstreamGet(ws, `/v1/threads/${threadId}`, new URLSearchParams(), PAGE_TTL_MS, validateThread, refresh);
}

export function fetchMessages(
  ws: RegistryWorkspace,
  threadId: string,
  params: PageParams,
  refresh = false,
): Promise<UpstreamResult<UpstreamPage<UpstreamMessage>>> {
  if (!isValidThreadId(threadId)) {
    return Promise.resolve({ ok: false, code: "not-found", status: 404, fresh: false });
  }
  return upstreamGet(
    ws,
    `/v1/threads/${threadId}/messages`,
    pageQuery(params),
    PAGE_TTL_MS,
    validatePageOf(validateMessage),
    refresh,
  );
}

export function fetchTags(
  ws: RegistryWorkspace,
  params: PageParams,
  refresh = false,
): Promise<UpstreamResult<UpstreamPage<UpstreamTagInfo>>> {
  return upstreamGet(ws, "/v1/tags", pageQuery(params), PAGE_TTL_MS, validatePageOf(validateTagInfo), refresh);
}

export function fetchUser(
  ws: RegistryWorkspace,
  username: string,
): Promise<UpstreamResult<UpstreamPublicUser>> {
  if (!isValidUsername(username)) {
    return Promise.resolve({ ok: false, code: "not-found", status: 404, fresh: false });
  }
  // Usernames match ^[a-z0-9][a-z0-9._-]{0,31}$ — safe as a path segment.
  return upstreamGet(ws, `/v1/users/${username}`, new URLSearchParams(), DISCOVERY_TTL_MS, validatePublicUser, false);
}
