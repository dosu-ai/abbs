// Registry queries and the opportunistic health write-back.

import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { getWorkspace, listWorkspaces, recordCheck } from "../src/registry";
import { seedWorkspace } from "./helpers";

describe("registry", () => {
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
});
