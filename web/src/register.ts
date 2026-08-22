// Workspace registration (WEBSITE_PLAN.md Phase 3): the directory's one
// public mutation. POST /api/workspaces (JSON) and the /add form share the
// same flow: normalize the submitted URL, dedupe against the registry,
// rate-limit per address, verify the public contract live (verify.ts), and
// insert the row as active. Failures return precise, bounded explanations —
// never a reflected upstream body.
//
// SSRF posture, layered:
//   Enforced here    — HTTPS-only public origins: no credentials, query,
//                      fragment, path, or explicit port; no IP literals
//                      (dotted, decimal, hex, IPv6); no single-label or
//                      special-use hostnames (.localhost, .local, .internal,
//                      .test, .invalid, .example, .onion, .arpa, ...). The
//                      seed path for loopback dev workspaces bypasses
//                      registration entirely (rows inserted via SQL).
//   Enforced in the  — no redirect is ever followed (redirect: "manual"),
//   read proxy         6s timeout, 1 MiB body cap, fixed request headers,
//                      ≤3 upstream GETs per submission.
//   Delegated        — the Workers runtime exposes no DNS resolver, so
//                      resolved-IP validation (private/loopback/link-local/
//                      metadata ranges, DNS rebinding) cannot happen in this
//                      layer; production relies on Cloudflare's egress not
//                      routing subrequests into private address space. A
//                      self-hosted `wrangler dev` deployment does not get
//                      that protection — do not expose local dev publicly.

import { workspaceJson } from "./api";
import { addPage } from "./pages/static";
import { jsonResponse, problemResponse } from "./problems";
import { findByBaseUrl, insertActive, slugTaken } from "./registry";
import type { Env, RegistryWorkspace } from "./types";
import { probeTarget, verifyWorkspace } from "./verify";
import type { VerifyErr } from "./verify";

// --- URL normalization -------------------------------------------------------

export type NormalizeReject =
  | "empty"
  | "too-long"
  | "unparseable"
  | "not-https"
  | "credentials"
  | "fragment"
  | "query"
  | "non-root-path"
  | "explicit-port"
  | "ip-literal"
  | "host-not-public";

export type NormalizeResult =
  | { ok: true; origin: string }
  | { ok: false; code: NormalizeReject };

const MAX_URL_CHARS = 512;

// Hostnames that can never be a public ABBS origin: special-use and
// reserved TLDs (RFC 2606, 6761, 6762, 8375, 9476) plus common intranet
// suffixes. Registration rejects them at the name layer since the runtime
// cannot check what they resolve to.
const BLOCKED_TLDS = new Set([
  "localhost",
  "local",
  "internal",
  "intranet",
  "home",
  "corp",
  "lan",
  "test",
  "invalid",
  "example",
  "onion",
  "alt",
  "arpa",
]);

// A public TLD label: alphabetic, or an IDN (punycode) label. Anything else
// (trailing numeric labels, hex) is an IP spelled creatively.
const TLD_RE = /^([a-z]{2,63}|xn--[a-z0-9-]{1,59})$/;
const LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

export function normalizeSubmission(raw: string): NormalizeResult {
  const trimmed = raw.trim();
  if (trimmed === "") return { ok: false, code: "empty" };
  if (trimmed.length > MAX_URL_CHARS) return { ok: false, code: "too-long" };

  // A bare hostname is unambiguous — it can only mean https.
  const withScheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(trimmed)
    ? trimmed
    : `https://${trimmed}`;

  let url: URL;
  try {
    url = new URL(withScheme);
  } catch {
    return { ok: false, code: "unparseable" };
  }

  if (url.protocol !== "https:") return { ok: false, code: "not-https" };
  if (url.username !== "" || url.password !== "") return { ok: false, code: "credentials" };
  if (url.hash !== "") return { ok: false, code: "fragment" };
  if (url.search !== "") return { ok: false, code: "query" };
  if (url.pathname !== "/" && url.pathname !== "") return { ok: false, code: "non-root-path" };
  if (url.port !== "") return { ok: false, code: "explicit-port" };

  // The URL parser has already lowercased and punycoded the hostname.
  const host = url.hostname.replace(/\.$/, "");
  if (host === "" || host.length > 253) return { ok: false, code: "unparseable" };
  if (host.startsWith("[")) return { ok: false, code: "ip-literal" }; // IPv6
  if (/^[0-9]+([.][0-9]+)*$/.test(host)) return { ok: false, code: "ip-literal" }; // dotted/decimal
  if (/^0x[0-9a-f]+$/.test(host)) return { ok: false, code: "ip-literal" }; // hex

  const labels = host.split(".");
  if (labels.length < 2) return { ok: false, code: "host-not-public" };
  if (!labels.every((l) => LABEL_RE.test(l))) return { ok: false, code: "unparseable" };
  const tld = labels[labels.length - 1];
  if (!TLD_RE.test(tld)) return { ok: false, code: "ip-literal" };
  if (BLOCKED_TLDS.has(tld)) return { ok: false, code: "host-not-public" };

  return { ok: true, origin: `https://${host}` };
}

// --- per-address submission rate limit ----------------------------------------
// Stricter than the refresh bucket: every allowed submission can cost up to
// three outbound verification requests. Per-isolate memory, like the other
// buckets; Turnstile is the documented escalation if abuse appears.

const SUBMIT_BURST = 3;
const SUBMIT_REFILL_MS = 300_000; // one submission credit per 5 minutes
const submitBuckets = new Map<string, { tokens: number; at: number }>();

export function allowSubmission(addr: string, now = Date.now()): boolean {
  if (submitBuckets.size > 4096) submitBuckets.clear();
  const b = submitBuckets.get(addr) ?? { tokens: SUBMIT_BURST, at: now };
  const refilled = Math.floor((now - b.at) / SUBMIT_REFILL_MS);
  const tokens = Math.min(SUBMIT_BURST, b.tokens + refilled);
  const at = tokens === SUBMIT_BURST ? now : b.at + refilled * SUBMIT_REFILL_MS;
  if (tokens < 1) {
    submitBuckets.set(addr, { tokens, at });
    return false;
  }
  submitBuckets.set(addr, { tokens: tokens - 1, at });
  return true;
}

// --- registration ---------------------------------------------------------

export type RegistrationOutcome =
  | { kind: "created"; ws: RegistryWorkspace }
  | { kind: "exists"; ws: RegistryWorkspace }
  | { kind: "invalid-url"; code: NormalizeReject }
  | { kind: "delisted" }
  | { kind: "rate-limited"; retryAfterSeconds: number }
  | { kind: "failed"; err: VerifyErr };

function slugify(v: string): string {
  return v
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48)
    .replace(/-+$/, "");
}

const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;

async function chooseSlug(
  db: D1Database,
  name: string,
  hostname: string,
  id: string,
): Promise<string> {
  let base = slugify(name);
  if (!SLUG_RE.test(base)) base = slugify(hostname.replace(/\./g, "-"));
  if (!SLUG_RE.test(base)) base = "board";
  const candidates = [base];
  for (let i = 2; i <= 9; i++) candidates.push(`${base}-${i}`);
  candidates.push(`${base}-${id.slice(0, 8)}`);
  for (const c of candidates) {
    if (!(await slugTaken(db, c))) return c;
  }
  return id; // a UUID is itself a valid, collision-free slug
}

// registerWorkspace is idempotent: resubmitting a listed URL returns the
// existing listing without spending rate-limit budget or touching the
// upstream, and a delisted URL is refused — registration never resurrects
// an operator-removed row.
export async function registerWorkspace(
  env: Env,
  addr: string,
  raw: string,
): Promise<RegistrationOutcome> {
  const norm = normalizeSubmission(raw);
  if (!norm.ok) return { kind: "invalid-url", code: norm.code };

  const existing = await findByBaseUrl(env.DB, norm.origin);
  if (existing !== null) {
    return existing.status === "delisted"
      ? { kind: "delisted" }
      : { kind: "exists", ws: existing };
  }

  if (!allowSubmission(addr)) {
    return { kind: "rate-limited", retryAfterSeconds: SUBMIT_REFILL_MS / 1000 };
  }

  const verified = await verifyWorkspace(probeTarget(norm.origin), {
    probeReads: true,
    requireCanonicalOrigin: norm.origin,
  });
  if (!verified.ok) return { kind: "failed", err: verified };

  const id = crypto.randomUUID();
  const now = new Date().toISOString();
  const hostname = new URL(norm.origin).hostname;
  for (let attempt = 0; attempt < 3; attempt++) {
    const slug = await chooseSlug(env.DB, verified.value.name, hostname, id);
    try {
      await insertActive(env.DB, {
        id,
        slug,
        baseUrl: norm.origin,
        canonicalUrl: verified.value.canonicalUrl,
        name: verified.value.name,
        description: verified.value.description,
        apiVersion: verified.value.apiVersion,
        now,
      });
    } catch {
      // Unique-constraint race: either the same URL landed concurrently
      // (return it) or the slug was taken between check and insert (retry).
      const raced = await findByBaseUrl(env.DB, norm.origin);
      if (raced !== null) {
        return raced.status === "delisted" ? { kind: "delisted" } : { kind: "exists", ws: raced };
      }
      continue;
    }
    const ws = await findByBaseUrl(env.DB, norm.origin);
    if (ws !== null) return { kind: "created", ws };
    break;
  }
  throw new Error("registration insert failed");
}

// --- error copy -----------------------------------------------------------
// Two renderings of the same bounded outcome: a lowercase problem detail for
// the JSON API and uppercase terminal copy for the form. Only our own codes
// and the schema-validated canonical URL ever appear.

const NORMALIZE_COPY: Record<NormalizeReject, { detail: string; form: string }> = {
  empty: { detail: "enter a workspace URL", form: "ENTER A WORKSPACE URL." },
  "too-long": {
    detail: "the URL is too long (512 characters max)",
    form: "THE URL IS TOO LONG (512 CHARACTERS MAX).",
  },
  unparseable: { detail: "that does not parse as a URL", form: "THAT DOES NOT PARSE AS A URL." },
  "not-https": {
    detail: "public boards must be served over HTTPS",
    form: "PUBLIC BOARDS MUST BE SERVED OVER HTTPS.",
  },
  credentials: {
    detail: "remove the embedded credentials from the URL",
    form: "REMOVE THE EMBEDDED CREDENTIALS FROM THE URL.",
  },
  fragment: {
    detail: "remove the #fragment — submit the bare origin",
    form: "REMOVE THE #FRAGMENT — SUBMIT THE BARE ORIGIN.",
  },
  query: {
    detail: "remove the query string — submit the bare origin",
    form: "REMOVE THE QUERY STRING — SUBMIT THE BARE ORIGIN.",
  },
  "non-root-path": {
    detail: "submit the server origin without a path",
    form: "SUBMIT THE SERVER ORIGIN WITHOUT A PATH.",
  },
  "explicit-port": {
    detail: "HTTPS on the default port only — remove the port",
    form: "HTTPS ON THE DEFAULT PORT ONLY — REMOVE THE PORT.",
  },
  "ip-literal": {
    detail: "submit a public DNS hostname, not an IP address",
    form: "SUBMIT A PUBLIC DNS HOSTNAME, NOT AN IP ADDRESS.",
  },
  "host-not-public": {
    detail: "the hostname must be a public DNS name (not single-label or reserved)",
    form: "THE HOSTNAME MUST BE A PUBLIC DNS NAME (NOT SINGLE-LABEL OR RESERVED).",
  },
};

function verifyDetail(err: VerifyErr): string {
  const base = `verification failed at ${err.stage}: ${err.code}`;
  return err.code === "canonical-mismatch" && err.declaredCanonical !== undefined
    ? `${base} (server declares ${err.declaredCanonical})`
    : base;
}

function verifyFormMessage(err: VerifyErr): string {
  switch (err.code) {
    case "wrong-api-version":
      return "THE SERVER DOES NOT SPEAK PROTOCOL VERSION v1.";
    case "not-public":
      return "THE WORKSPACE DOES NOT DECLARE visibility: public.";
    case "listing-revoked":
      return "THE WORKSPACE DOES NOT CONSENT TO LISTING — SET directory_listing: true.";
    case "no-description":
      return "A LISTED WORKSPACE NEEDS A NON-EMPTY PLAIN-TEXT DESCRIPTION IN /v1/server.";
    case "bad-canonical":
      return "THE WORKSPACE MUST DECLARE AN HTTPS canonical_url IN /v1/server.";
    case "canonical-mismatch":
      return `THE SERVER DECLARES ${err.declaredCanonical ?? "A DIFFERENT URL"} AS ITS CANONICAL URL — SUBMIT THAT URL.`;
    case "private-thread-leak":
      return "THE ANONYMOUS THREAD LIST EXPOSED A NON-PUBLIC THREAD — REGISTRATION REFUSED.";
    default:
      break;
  }
  const what =
    err.stage === "discovery"
      ? "GET /v1/server"
      : err.stage === "thread-list"
        ? "THE ANONYMOUS THREAD LIST PROBE"
        : "THE ANONYMOUS MESSAGE LIST PROBE";
  if (err.unreachable) return `${what} DID NOT ANSWER (${err.code.toUpperCase()}).`;
  if (err.code === "redirect") {
    return `${what} REDIRECTED — VERIFICATION NEVER FOLLOWS REDIRECTS. SERVE THE PROTOCOL AT THE SUBMITTED ORIGIN.`;
  }
  if (err.code === "rate-limited") return "THE WORKSPACE RATE LIMITED VERIFICATION — RETRY SHORTLY.";
  if (err.code === "not-found") {
    return `${what} RETURNED 404 — IS THIS AN ABBS SERVER WITH PUBLIC READS ENABLED?`;
  }
  if (err.code === "http-4xx") {
    return `${what} WAS REFUSED — ANONYMOUS READS MUST WORK WITHOUT A TOKEN.`;
  }
  return `${what} DID NOT RETURN A VALID RESPONSE (${err.code.toUpperCase()}).`;
}

const DELISTED_DETAIL = "this workspace was removed by the directory operators";
const DELISTED_FORM = "THIS WORKSPACE WAS REMOVED BY THE DIRECTORY OPERATORS AND CANNOT BE RESUBMITTED.";
const RATE_DETAIL = "too many submissions from this address";
const RATE_FORM = "TOO MANY SUBMISSIONS FROM YOUR ADDRESS — TRY AGAIN IN A FEW MINUTES.";

// --- HTTP adapters ----------------------------------------------------------

const MAX_BODY_CHARS = 4096;

async function readBody(request: Request): Promise<string | null> {
  const len = request.headers.get("Content-Length");
  if (len !== null && /^\d+$/.test(len) && Number(len) > MAX_BODY_CHARS) return null;
  // Bytes, not .text(): the cap is byte-accurate and workerd does not warn
  // about decoding urlencoded bodies.
  const bytes = await request.arrayBuffer();
  if (bytes.byteLength > MAX_BODY_CHARS) return null;
  return new TextDecoder().decode(bytes);
}

function clientAddr(request: Request): string {
  return request.headers.get("CF-Connecting-IP") ?? "unknown";
}

function listingUrl(ws: RegistryWorkspace): string {
  return `/w/${ws.slug}`;
}

// POST /api/workspaces — the JSON face of registration. 201 on first
// listing, 200 on idempotent resubmission, RFC 9457 problems otherwise.
export async function apiRegisterWorkspace(request: Request, env: Env): Promise<Response> {
  const body = await readBody(request);
  if (body === null) return problemResponse(400, "validation", "request body too large");

  const ct = request.headers.get("Content-Type") ?? "";
  let raw: string | null = null;
  if (/^application\/json\b/i.test(ct)) {
    try {
      const parsed: unknown = JSON.parse(body);
      if (
        typeof parsed === "object" &&
        parsed !== null &&
        typeof (parsed as { url?: unknown }).url === "string"
      ) {
        raw = (parsed as { url: string }).url;
      }
    } catch {
      raw = null;
    }
  } else if (/^application\/x-www-form-urlencoded\b/i.test(ct)) {
    raw = new URLSearchParams(body).get("url");
  }
  if (raw === null) {
    return problemResponse(400, "validation", 'body must be JSON {"url": "https://…"} or a url form field');
  }

  const outcome = await registerWorkspace(env, clientAddr(request), raw);
  switch (outcome.kind) {
    case "created":
      return jsonResponse(
        201,
        { workspace: workspaceJson(outcome.ws), url: listingUrl(outcome.ws) },
        { Location: listingUrl(outcome.ws) },
      );
    case "exists":
      return jsonResponse(200, {
        workspace: workspaceJson(outcome.ws),
        url: listingUrl(outcome.ws),
      });
    case "invalid-url":
      return problemResponse(400, "validation", NORMALIZE_COPY[outcome.code].detail);
    case "delisted":
      return problemResponse(409, "delisted", DELISTED_DETAIL);
    case "rate-limited":
      return problemResponse(429, "rate-limited", RATE_DETAIL, {
        "Retry-After": String(outcome.retryAfterSeconds),
      });
    case "failed":
      return problemResponse(422, "registration-failed", verifyDetail(outcome.err));
  }
}

// POST /add — the no-JS form flow: 303 to the listing on success
// (POST-redirect-GET), or the form re-rendered with a precise error and the
// submitted value preserved.
export async function formRegisterWorkspace(request: Request, env: Env): Promise<Response> {
  const body = await readBody(request);
  if (body === null) {
    return addPage({ error: "THE SUBMISSION WAS TOO LARGE.", status: 400 });
  }
  const raw = new URLSearchParams(body).get("url") ?? "";
  const outcome = await registerWorkspace(env, clientAddr(request), raw);

  if (outcome.kind === "created" || outcome.kind === "exists") {
    return new Response(null, {
      status: 303,
      headers: { Location: new URL(listingUrl(outcome.ws), request.url).href },
    });
  }

  const value = raw.slice(0, MAX_URL_CHARS);
  switch (outcome.kind) {
    case "invalid-url":
      return addPage({ error: NORMALIZE_COPY[outcome.code].form, value, status: 400 });
    case "delisted":
      return addPage({ error: DELISTED_FORM, value, status: 409 });
    case "rate-limited":
      return addPage({ error: RATE_FORM, value, status: 429 });
    case "failed":
      return addPage({ error: verifyFormMessage(outcome.err), value, status: 422 });
  }
}
