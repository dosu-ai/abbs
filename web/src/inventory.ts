// URL-only public-thread inventory for crawler discovery. This is the one
// narrow exception to the registry's no-content persistence rule: only
// workspace/thread IDs and discovery/last-seen timestamps are stored.

import type { RegistryWorkspace } from "./types";
import { fetchThreads } from "./upstream";
import type { UpstreamErrorCode } from "./upstream";
import type { VerifyErrorCode } from "./verify";

const PAGE_BUDGET = 4;

export interface InventoryResult {
  pages: number;
  urls: number;
  phase: RegistryWorkspace["inventoryPhase"];
  completed: boolean;
  errorCode?: VerifyErrorCode;
}

export async function runWorkspaceInventory(
  db: D1Database,
  ws: RegistryWorkspace,
  now: string,
): Promise<InventoryResult> {
  let phase = ws.inventoryPhase;
  let cursor = ws.inventoryCursor;
  let anchor = ws.inventoryAnchor;
  let completedAt = ws.inventoryCompletedAt;
  let pages = 0;
  let urls = 0;
  let completed = false;

  while (pages < PAGE_BUDGET) {
    if (phase !== "bootstrap" && cursor === null && anchor === null) {
      phase = "bootstrap";
    }

    const startsScan = cursor === null;
    const result = await fetchThreads(
      ws,
      {
        limit: 100,
        ...(cursor !== null ? { page: cursor } : {}),
        ...(cursor === null && phase !== "bootstrap" && anchor !== null ? { since: anchor } : {}),
      },
      true,
    );
    if (!result.ok) {
      return { pages, urls, phase, completed, errorCode: result.code as UpstreamErrorCode };
    }
    pages++;

    if (result.value.items.some((thread) => thread.kind !== "public")) {
      return { pages, urls, phase, completed, errorCode: "private-thread-leak" };
    }

    if (startsScan) anchor = result.value.as_of;
    cursor = result.value.next_page;
    urls += result.value.items.length;

    const statements: D1PreparedStatement[] = result.value.items.map((thread) =>
      db
        .prepare(
          `INSERT INTO public_thread_urls (workspace_id, thread_id, discovered_at, last_seen_at)
           SELECT ?, ?, ?, ?
           WHERE EXISTS (
             SELECT 1 FROM workspaces
             WHERE id = ? AND status != 'delisted' AND search_eligible = 1
           )
           ON CONFLICT(workspace_id, thread_id) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
        )
        .bind(ws.id, thread.id, now, now, ws.id),
    );

    if (cursor === null) {
      if (phase === "bootstrap") {
        phase = "catchup";
      } else {
        phase = "incremental";
        completedAt = now;
        completed = true;
      }
    }

    statements.push(
      db
        .prepare(
          `UPDATE workspaces SET
             inventory_phase = ?, inventory_cursor = ?, inventory_anchor = ?,
             inventory_completed_at = ?
           WHERE id = ? AND status != 'delisted' AND search_eligible = 1`,
        )
        .bind(phase, cursor, anchor, completedAt, ws.id),
    );
    await db.batch(statements);
    if (completed) break;
  }

  return { pages, urls, phase, completed };
}
