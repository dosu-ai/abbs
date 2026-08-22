// Registry queries and the opportunistic health write-back.

import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import {
  findByBaseUrl,
  getWorkspace,
  listWorkspaces,
  markDelisted,
  recordCheck,
  relistWorkspace,
} from "../src/registry";
import { seedWorkspace } from "./helpers";

describe("registry", () => {
  it("defaults pre-search rows to an unqualified bootstrap state", async () => {
    await env.DB.prepare(
      `INSERT INTO workspaces (id, slug, base_url, name, description, status, submitted_at)
       VALUES (?, ?, ?, ?, ?, 'active', ?)`,
    )
      .bind(
        "0198c0de-0000-7000-8000-999999999999",
        "legacy-search-defaults",
        "https://legacy-search-defaults.example",
        "legacy",
        "legacy row",
        "2026-08-22T00:00:00Z",
      )
      .run();
    expect(await getWorkspace(env.DB, "legacy-search-defaults")).toMatchObject({
      searchEligible: false,
      searchSuccessCount: 0,
      searchEligibleAt: null,
      searchContentFound: false,
      inventoryPhase: "bootstrap",
      inventoryCursor: null,
      inventoryAnchor: null,
      inventoryCompletedAt: null,
    });
    await env.DB.prepare("DELETE FROM workspaces WHERE slug = 'legacy-search-defaults'").run();
  });

  it("lists workspaces ordered by name, excluding delisted", async () => {
    await seedWorkspace({ name: "zulu" });
    await seedWorkspace({ name: "Alpha" });
    const gone = await seedWorkspace({ name: "hidden", status: "delisted" });
    const names = (await listWorkspaces(env.DB)).map((w) => w.name);
    expect(names).toEqual(["Alpha", "zulu"]);
    expect(await getWorkspace(env.DB, gone.slug)).toBeNull();
  });

  it("records a successful check and refreshes cached metadata", async () => {
    const ws = await seedWorkspace({ status: "pending" });
    await recordCheck(env.DB, ws.id, "2026-08-22T12:00:00Z", {
      ok: true,
      name: "fresh name",
      description: "fresh description",
      apiVersion: "v1",
      canonicalUrl: "https://fresh.example",
    });
    const got = await getWorkspace(env.DB, ws.slug);
    expect(got).not.toBeNull();
    expect(got?.status).toBe("active");
    expect(got?.name).toBe("fresh name");
    expect(got?.description).toBe("fresh description");
    expect(got?.canonicalUrl).toBe("https://fresh.example");
    expect(got?.lastSuccessAt).toBe("2026-08-22T12:00:00Z");
    expect(got?.lastErrorCode).toBeNull();
  });

  it("records failures with bounded codes and the right status", async () => {
    const ws = await seedWorkspace();
    await recordCheck(env.DB, ws.id, "2026-08-22T12:00:00Z", {
      ok: false,
      errorCode: "timeout",
      unreachable: true,
    });
    let got = await getWorkspace(env.DB, ws.slug);
    expect(got?.status).toBe("unreachable");
    expect(got?.lastErrorCode).toBe("timeout");

    await recordCheck(env.DB, ws.id, "2026-08-22T12:01:00Z", {
      ok: false,
      errorCode: "not-public",
    });
    got = await getWorkspace(env.DB, ws.slug);
    expect(got?.status).toBe("degraded");
    expect(got?.lastErrorCode).toBe("not-public");
  });

  it("finds rows by base URL including delisted ones", async () => {
    const ws = await seedWorkspace({ status: "delisted" });
    expect((await findByBaseUrl(env.DB, ws.baseUrl))?.id).toBe(ws.id);
    expect(await findByBaseUrl(env.DB, "https://nowhere.example")).toBeNull();
  });

  it("delists with a bounded code and preserves the first delist reason", async () => {
    const ws = await seedWorkspace();
    await markDelisted(env.DB, ws.id, "2026-08-22T12:00:00Z", "operator-removed");
    let row = await env.DB.prepare("SELECT status, last_error_code FROM workspaces WHERE id = ?")
      .bind(ws.id)
      .first<{ status: string; last_error_code: string }>();
    expect(row).toEqual({ status: "delisted", last_error_code: "operator-removed" });

    // A second delist (e.g. the sweep racing the operator) keeps the reason.
    await markDelisted(env.DB, ws.id, "2026-08-22T12:01:00Z", "listing-revoked");
    row = await env.DB.prepare("SELECT status, last_error_code FROM workspaces WHERE id = ?")
      .bind(ws.id)
      .first<{ status: string; last_error_code: string }>();
    expect(row?.last_error_code).toBe("operator-removed");
  });

  it("never resurrects a delisted workspace", async () => {
    const ws = await seedWorkspace({ status: "delisted" });
    await recordCheck(env.DB, ws.id, "2026-08-22T12:00:00Z", {
      ok: true,
      name: "sneaky",
      description: "d",
    });
    const row = await env.DB.prepare("SELECT status FROM workspaces WHERE id = ?")
      .bind(ws.id)
      .first<{ status: string }>();
    expect(row?.status).toBe("delisted");
  });

  it("does not reset or delete inventory when relist is called for an active row", async () => {
    const ws = await seedWorkspace({ searchEligible: true, searchSuccessCount: 2 });
    await env.DB.prepare(
      "INSERT INTO public_thread_urls (workspace_id, thread_id, discovered_at, last_seen_at) VALUES (?, ?, ?, ?)",
    )
      .bind(
        ws.id,
        "0198aaaa-bbbb-7ccc-8ddd-eeeeffff0001",
        "2026-08-22T00:00:00Z",
        "2026-08-22T00:00:00Z",
      )
      .run();
    await relistWorkspace(env.DB, ws.id);
    expect((await getWorkspace(env.DB, ws.slug))?.status).toBe("active");
    expect(
      await env.DB.prepare("SELECT COUNT(*) AS n FROM public_thread_urls WHERE workspace_id = ?")
        .bind(ws.id)
        .first<number>("n"),
    ).toBe(1);
  });
});
