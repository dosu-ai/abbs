// Verification of the public directory contract (WEBSITE_PLAN.md
// "Registration flow"), shared by registration and the scheduled sweep.
//
// A conforming workspace answers anonymous GET /v1/server with a valid
// discovery document declaring api_version v1, public visibility, an HTTPS
// canonical URL, directory-listing consent, and a non-empty plain-text
// description. Registration additionally probes the anonymous thread list
// and one message list, and requires the declared canonical origin to match
// the submitted origin (a mirror or proxy of someone else's server fails
// with a precise error instead of being listed under the wrong URL).
//
// All upstream traffic goes through the constrained read proxy in
// upstream.ts, so every probe inherits the same HTTPS rule, header
// stripping, no-redirect policy, time/size caps, and bounded error codes.

import { listForVerification, markDelisted, recordCheck } from "./registry";
import { fetchDiscovery, fetchMessages, fetchThreads } from "./upstream";
import type { UpstreamErrorCode } from "./upstream";
import type { Env, RegistryWorkspace } from "./types";

// Contract violations found in an otherwise well-formed discovery document.
// Together with UpstreamErrorCode these are the only values that ever reach
// the registry's last_error_code column or a visitor-facing error message.
export type ContractErrorCode =
  | "wrong-api-version"
  | "not-public"
  | "listing-revoked"
  | "no-description"
  | "bad-canonical"
  | "canonical-mismatch"
  | "private-thread-leak";

export type VerifyErrorCode = UpstreamErrorCode | ContractErrorCode;
export type VerifyStage = "discovery" | "thread-list" | "message-list";

export interface VerifiedMetadata {
  name: string;
  description: string;
  apiVersion: string;
  canonicalUrl: string;
}

export interface VerifyOk {
  ok: true;
  value: VerifiedMetadata;
}

export interface VerifyErr {
  ok: false;
  stage: VerifyStage;
  code: VerifyErrorCode;
  unreachable: boolean;
  // True when the server is reachable and public but withdrew (or never
  // gave) directory_listing consent — the one condition that delists.
  consentRevoked: boolean;
  // On canonical-mismatch: the canonical URL the server declared (schema-
  // validated, length-capped; escaped at render like all upstream values).
  declaredCanonical?: string;
}

export type VerifyResult = VerifyOk | VerifyErr;

function fail(stage: VerifyStage, code: VerifyErrorCode): VerifyErr {
  return {
    ok: false,
    stage,
    code,
    unreachable: code === "timeout" || code === "network",
    consentRevoked: false,
  };
}

// probeTarget wraps a normalized origin in registry-row shape so the read
// proxy can be pointed at a not-yet-registered workspace during
// registration. The synthetic id keeps its cache entries away from real rows.
export function probeTarget(origin: string): RegistryWorkspace {
  return {
    id: `probe ${origin}`,
    slug: "probe",
    baseUrl: origin,
    canonicalUrl: null,
    name: "",
    description: "",
    apiVersion: null,
    status: "pending",
    submittedAt: "",
    lastCheckedAt: null,
    lastSuccessAt: null,
    lastErrorCode: null,
  };
}

export interface VerifyOptions {
  // Registration probes anonymous reads; the scheduled sweep repeats
  // discovery only.
  probeReads: boolean;
  // Registration requires the declared canonical origin to equal the
  // submitted origin; the sweep passes null and caches whatever the
  // authoritative server now declares.
  requireCanonicalOrigin: string | null;
}

// verifyWorkspace always talks to the live upstream (refresh bypasses the
// short discovery cache): a verification verdict must never come from a
// stale cache entry.
export async function verifyWorkspace(
  ws: RegistryWorkspace,
  opts: VerifyOptions,
): Promise<VerifyResult> {
  const d = await fetchDiscovery(ws, true);
  if (!d.ok) return fail("discovery", d.code);

  const info = d.value;
  const w = info.workspace;
  if (info.api_version !== "v1") return fail("discovery", "wrong-api-version");
  if (w.visibility !== "public") return fail("discovery", "not-public");
  if (w.directory_listing !== true) {
    return { ...fail("discovery", "listing-revoked"), consentRevoked: true };
  }
  if ((w.description ?? "").trim() === "") return fail("discovery", "no-description");

  const declared = w.canonical_url;
  if (declared === undefined) return fail("discovery", "bad-canonical");
  let canonicalOrigin: string;
  try {
    const cu = new URL(declared);
    if (cu.protocol !== "https:") return fail("discovery", "bad-canonical");
    canonicalOrigin = cu.origin;
  } catch {
    return fail("discovery", "bad-canonical");
  }
  if (opts.requireCanonicalOrigin !== null && canonicalOrigin !== opts.requireCanonicalOrigin) {
    return { ...fail("discovery", "canonical-mismatch"), declaredCanonical: declared };
  }

  if (opts.probeReads) {
    const threads = await fetchThreads(ws, { limit: 5 }, true);
    if (!threads.ok) return fail("thread-list", threads.code);
    // The anonymous surface serves public threads only; anything else in
    // the list is a private-data anomaly and refuses registration.
    for (const t of threads.value.items) {
      if (t.kind !== "public") return fail("thread-list", "private-thread-leak");
    }
    const first = threads.value.items[0];
    if (first !== undefined) {
      const messages = await fetchMessages(ws, first.id, { limit: 1 }, true);
      if (!messages.ok) return fail("message-list", messages.code);
    }
  }

  return {
    ok: true,
    value: {
      name: w.name,
      description: w.description ?? "",
      apiVersion: info.api_version,
      canonicalUrl: declared,
    },
  };
}

// --- the scheduled sweep -----------------------------------------------------
// Cron-driven re-verification (wrangler.jsonc triggers). This replaces the
// Phase 2 opportunistic health write-back: page reads stay pure reads, and
// the registry's status/health columns change only here and at registration.
//
//   - conforming        -> active, cached metadata refreshed from /v1/server
//   - consent withdrawn -> delisted (the only automatic delisting)
//   - any other failure -> degraded or unreachable; the listing survives
//   - delisted rows are never contacted and never resurrected

const SWEEP_CONCURRENCY = 5;

export interface SweepSummary {
  checked: number;
  healthy: number;
  degraded: number;
  unreachable: number;
  delisted: number;
}

export async function runVerificationSweep(env: Env): Promise<SweepSummary> {
  const rows = await listForVerification(env.DB);
  const now = new Date().toISOString();
  const summary: SweepSummary = {
    checked: rows.length,
    healthy: 0,
    degraded: 0,
    unreachable: 0,
    delisted: 0,
  };

  for (let i = 0; i < rows.length; i += SWEEP_CONCURRENCY) {
    await Promise.all(
      rows.slice(i, i + SWEEP_CONCURRENCY).map(async (ws) => {
        const r = await verifyWorkspace(ws, { probeReads: false, requireCanonicalOrigin: null });
        if (r.ok) {
          await recordCheck(env.DB, ws.id, now, {
            ok: true,
            name: r.value.name,
            description: r.value.description,
            apiVersion: r.value.apiVersion,
            canonicalUrl: r.value.canonicalUrl,
          });
          summary.healthy++;
          return;
        }
        if (r.consentRevoked) {
          await markDelisted(env.DB, ws.id, now, "listing-revoked");
          summary.delisted++;
          return;
        }
        await recordCheck(env.DB, ws.id, now, {
          ok: false,
          errorCode: r.code,
          unreachable: r.unreachable,
        });
        if (r.unreachable) summary.unreachable++;
        else summary.degraded++;
      }),
    );
  }
  return summary;
}
