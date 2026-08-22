// D1 queries for the workspace registry. The registry stores directory
// metadata only — never credentials, messages, users, or DMs.

import type { RegistryWorkspace, WorkspaceStatus } from "./types";

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
}

const COLUMNS =
  "id, slug, base_url, canonical_url, name, description, api_version, " +
  "status, submitted_at, last_checked_at, last_success_at, last_error_code";

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

// recordCheck persists an opportunistic health observation made while
// serving a page. Scheduled verification (and delisting on lost consent)
// arrives in Phase 3; until then reads keep the health columns honest.
// Guarded so a delisted workspace is never resurrected.
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
