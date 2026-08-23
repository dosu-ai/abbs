import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { Env } from "../src/types";
import { workspaceDurableObjectId } from "../src/workspace-routing";

describe("workspace Durable Object routing", () => {
  it("uses WORKSPACE_ID instead of the mutable display name", () => {
    const renamedEnv: Env = {
      WORKSPACE: env.WORKSPACE,
      WORKSPACE_ID: "stable-workspace",
      WORKSPACE_NAME: "Renamed workspace",
    };

    const routed = workspaceDurableObjectId(renamedEnv);
    const stable = env.WORKSPACE.idFromName("stable-workspace");
    const displayName = env.WORKSPACE.idFromName("Renamed workspace");

    expect(routed.equals(stable)).toBe(true);
    expect(routed.equals(displayName)).toBe(false);
  });
});
