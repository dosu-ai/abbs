// D1 queries for the workspace registry. Workspace rows store directory
// metadata only; the separate URL inventory stores IDs/timestamps only.

import type { InventoryPhase, RegistryWorkspace, WorkspaceStatus } from "./types";

interface Row {
  id: string;
  slug: string;
  base_url: string;
  canonical_url: string | null;
  name: string;
  description: string;
  api_version: string | null;
  status: WorkspaceStatus;
  submitted_at: string;
  last_checked_at: string | null;
  last_success_at: string | null;
  last_error_code: string | null;
  search_eligible: number;
  search_success_count: number;
  search_eligible_at: string | null;
  search_content_found: number;
  inventory_phase: InventoryPhase;
  inventory_cursor: string | null;
  inventory_anchor: string | null;
  inventory_completed_at: string | null;
}

const COLUMNS =
  "id, slug, base_url, canonical_url, name, description, api_version, " +
  "status, submitted_at, last_checked_at, last_success_at, last_error_code, " +
  "search_eligible, search_success_count, search_eligible_at, search_content_found, " +
  "inventory_phase, inventory_cursor, inventory_anchor, inventory_completed_at";

function fromRow(r: Row): RegistryWorkspace {
  return {
    id: r.id,
    slug: r.slug,
    baseUrl: r.base_url,
    canonicalUrl: r.canonical_url,
    name: r.name,
    description: r.description,
    apiVersion: r.api_version,
    status: r.status,
    submittedAt: r.submitted_at,
    lastCheckedAt: r.last_checked_at,
    lastSuccessAt: r.last_success_at,
    lastErrorCode: r.last_error_code,
    searchEligible: r.search_eligible === 1,
    searchSuccessCount: r.search_success_count,
    searchEligibleAt: r.search_eligible_at,
    searchContentFound: r.search_content_found === 1,
    inventoryPhase: r.inventory_phase,
    inventoryCursor: r.inventory_cursor,
    inventoryAnchor: r.inventory_anchor,
    inventoryCompletedAt: r.inventory_completed_at,
  };
}

// listWorkspaces returns every listed (non-delisted) workspace, ordered by
// display name for a stable directory. Delisting is an operator action; a
// delisted row keeps its slug reserved but never renders.
export async function listWorkspaces(db: D1Database): Promise<RegistryWorkspace[]> {
  const rs = await db
    .prepare(
      `SELECT ${COLUMNS} FROM workspaces WHERE status != 'delisted'
       ORDER BY name COLLATE NOCASE ASC, slug ASC`,
    )
    .all<Row>();
  return rs.results.map(fromRow);
}

export async function getWorkspace(
  db: D1Database,
  slug: string,
): Promise<RegistryWorkspace | null> {
  const r = await db
    .prepare(`SELECT ${COLUMNS} FROM workspaces WHERE slug = ? AND status != 'delisted'`)
    .bind(slug)
    .first<Row>();
  return r === null ? null : fromRow(r);
}

// Includes delisted rows so stable public URLs can distinguish a retired
// listing (410) from an identifier that never existed (404).
export async function getWorkspaceBySlug(
  db: D1Database,
  slug: string,
): Promise<RegistryWorkspace | null> {
  const r = await db
    .prepare(`SELECT ${COLUMNS} FROM workspaces WHERE slug = ?`)
    .bind(slug)
    .first<Row>();
  return r === null ? null : fromRow(r);
}

// findByBaseUrl looks a row up by normalized base URL *including delisted
// rows* — registration must see a delisted row to refuse resurrecting it.
export async function findByBaseUrl(
  db: D1Database,
  baseUrl: string,
): Promise<RegistryWorkspace | null> {
  const r = await db
    .prepare(`SELECT ${COLUMNS} FROM workspaces WHERE base_url = ?`)
    .bind(baseUrl)
    .first<Row>();
  return r === null ? null : fromRow(r);
}

// listForVerification feeds the scheduled sweep: every row except delisted
// ones, which are never contacted again.
export async function listForVerification(db: D1Database): Promise<RegistryWorkspace[]> {
  const rs = await db
    .prepare(`SELECT ${COLUMNS} FROM workspaces WHERE status != 'delisted' ORDER BY slug ASC`)
    .all<Row>();
  return rs.results.map(fromRow);
}

// Slugs stay reserved even after delisting, so a removed workspace's URLs
// never start pointing at a different board.
export async function slugTaken(db: D1Database, slug: string): Promise<boolean> {
  const r = await db
    .prepare("SELECT 1 AS one FROM workspaces WHERE slug = ?")
    .bind(slug)
    .first<{ one: number }>();
  return r !== null;
}

export interface NewWorkspace {
  id: string;
  slug: string;
  baseUrl: string;
  canonicalUrl: string;
  name: string;
  description: string;
  apiVersion: string;
  now: string;
}

// insertActive inserts a freshly verified workspace. Unique-constraint
// races (base_url or slug) throw; the caller re-checks by base URL.
export async function insertActive(db: D1Database, ws: NewWorkspace): Promise<void> {
  await db
    .prepare(
      `INSERT INTO workspaces
         (id, slug, base_url, canonical_url, name, description, api_version,
          status, submitted_at, last_checked_at, last_success_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
    )
    .bind(
      ws.id,
      ws.slug,
      ws.baseUrl,
      ws.canonicalUrl,
      ws.name,
      ws.description,
      ws.apiVersion,
      ws.now,
      ws.now,
      ws.now,
    )
    .run();
}

// markDelisted takes a listing out of the directory (lost consent, or the
// operator statements documented in the README). The guard keeps the first
// delist reason intact if it races with itself.
export async function markDelisted(
  db: D1Database,
  id: string,
  now: string,
  code: string,
): Promise<void> {
  await db.batch([
    db
      .prepare(
        `UPDATE workspaces SET
           status = 'delisted', last_checked_at = ?, last_error_code = ?,
           search_eligible = 0, search_success_count = 0,
           search_eligible_at = NULL, search_content_found = 0,
           inventory_phase = 'bootstrap', inventory_cursor = NULL,
           inventory_anchor = NULL, inventory_completed_at = NULL
         WHERE id = ? AND status != 'delisted'`,
      )
      .bind(now, code, id),
    db.prepare("DELETE FROM public_thread_urls WHERE workspace_id = ?").bind(id),
  ]);
}

// Relisting is operator-only. It deliberately preserves the stable row and
// slug while deleting all URL inventory and restarting qualification.
export async function relistWorkspace(db: D1Database, id: string): Promise<void> {
  await db.batch([
    db
      .prepare(
        `DELETE FROM public_thread_urls
         WHERE workspace_id = ?
           AND EXISTS (SELECT 1 FROM workspaces WHERE id = ? AND status = 'delisted')`,
      )
      .bind(id, id),
    db
      .prepare(
        `UPDATE workspaces SET
           status = 'pending', last_error_code = NULL,
           search_eligible = 0, search_success_count = 0,
           search_eligible_at = NULL, search_content_found = 0,
           inventory_phase = 'bootstrap', inventory_cursor = NULL,
           inventory_anchor = NULL, inventory_completed_at = NULL
         WHERE id = ? AND status = 'delisted'`,
      )
      .bind(id),
  ]);
}

export interface CheckOutcome {
  ok: boolean;
  // Present on success: values re-verified from the authoritative /v1/server.
  name?: string;
  description?: string;
  apiVersion?: string;
  canonicalUrl?: string | null;
  // Present on failure: a bounded error code (see upstream.ts), and whether
  // the failure was a transport failure (unreachable) or a protocol-level
  // problem (degraded).
  errorCode?: string;
  unreachable?: boolean;
}

// recordCheck persists a scheduled-sweep observation (verify.ts). Guarded
// so a delisted workspace is never resurrected: delisting is reversed only
// by the operator relist statement documented in the README.
export async function recordCheck(
  db: D1Database,
  id: string,
  now: string,
  outcome: CheckOutcome,
): Promise<void> {
  if (outcome.ok) {
    await db
      .prepare(
        `UPDATE workspaces SET
           status = 'active', name = ?, description = ?, api_version = ?,
           canonical_url = ?, last_checked_at = ?, last_success_at = ?,
           last_error_code = NULL
         WHERE id = ? AND status != 'delisted'`,
      )
      .bind(
        outcome.name ?? "",
        outcome.description ?? "",
        outcome.apiVersion ?? null,
        outcome.canonicalUrl ?? null,
        now,
        now,
        id,
      )
      .run();
    return;
  }
  const status: WorkspaceStatus = outcome.unreachable ? "unreachable" : "degraded";
  await db
    .prepare(
      `UPDATE workspaces SET status = ?, last_checked_at = ?, last_error_code = ?
       WHERE id = ? AND status != 'delisted'`,
    )
    .bind(status, now, outcome.errorCode ?? "unknown", id)
    .run();
}

const QUALIFICATION_INTERVAL_MS = 15 * 60 * 1000;

export interface QualificationChange {
  becameEligible: boolean;
  becameSuspended: boolean;
  searchEligible: boolean;
  searchSuccessCount: number;
  searchContentFound: boolean;
}

// Registration verification never calls this function. Only scheduled
// checks advance the qualification streak.
export async function recordScheduledSuccess(
  db: D1Database,
  ws: RegistryWorkspace,
  now: string,
  outcome: Omit<CheckOutcome, "ok"> & { contentFound: boolean },
): Promise<QualificationChange> {
  const nowMs = Date.parse(now);
  const previousMs = ws.lastCheckedAt === null ? Number.NaN : Date.parse(ws.lastCheckedAt);
  const spaced =
    ws.searchSuccessCount === 0 ||
    (!Number.isNaN(nowMs) && !Number.isNaN(previousMs) && nowMs - previousMs >= QUALIFICATION_INTERVAL_MS);
  const count = spaced ? Math.min(2, ws.searchSuccessCount + 1) : ws.searchSuccessCount;
  const contentFound = ws.searchContentFound || outcome.contentFound;
  const eligible = ws.searchEligible || (count >= 2 && contentFound);
  const becameEligible = !ws.searchEligible && eligible;

  await db
    .prepare(
      `UPDATE workspaces SET
         status = 'active', name = ?, description = ?, api_version = ?, canonical_url = ?,
         last_checked_at = ?, last_success_at = ?, last_error_code = NULL,
         search_success_count = ?, search_content_found = ?, search_eligible = ?,
         search_eligible_at = CASE WHEN ? = 1 THEN COALESCE(search_eligible_at, ?) ELSE NULL END
       WHERE id = ? AND status != 'delisted'`,
    )
    .bind(
      outcome.name ?? "",
      outcome.description ?? "",
      outcome.apiVersion ?? null,
      outcome.canonicalUrl ?? null,
      now,
      now,
      count,
      contentFound ? 1 : 0,
      eligible ? 1 : 0,
      eligible ? 1 : 0,
      now,
      ws.id,
    )
    .run();

  return {
    becameEligible,
    becameSuspended: false,
    searchEligible: eligible,
    searchSuccessCount: count,
    searchContentFound: contentFound,
  };
}

// Only timeouts, network failures, rate limits, and upstream 5xx responses
// are transient. They reset a pending streak but do not oscillate an already
// indexed workspace out of search. Every deterministic failure suspends it.
export async function recordScheduledFailure(
  db: D1Database,
  ws: RegistryWorkspace,
  now: string,
  outcome: { errorCode: string; unreachable: boolean; transient: boolean },
): Promise<QualificationChange> {
  const preserveEligibility = outcome.transient && ws.searchEligible;
  const contentFound = outcome.transient ? ws.searchContentFound : false;
  const status: WorkspaceStatus = outcome.unreachable ? "unreachable" : "degraded";
  await db
    .prepare(
      `UPDATE workspaces SET
         status = ?, last_checked_at = ?, last_error_code = ?,
         search_eligible = ?, search_success_count = ?,
         search_eligible_at = ?, search_content_found = ?
       WHERE id = ? AND status != 'delisted'`,
    )
    .bind(
      status,
      now,
      outcome.errorCode,
      preserveEligibility ? 1 : 0,
      preserveEligibility ? ws.searchSuccessCount : 0,
      preserveEligibility ? ws.searchEligibleAt : null,
      contentFound ? 1 : 0,
      ws.id,
    )
    .run();
  return {
    becameEligible: false,
    becameSuspended: ws.searchEligible && !preserveEligibility,
    searchEligible: preserveEligibility,
    searchSuccessCount: preserveEligibility ? ws.searchSuccessCount : 0,
    searchContentFound: contentFound,
  };
}
