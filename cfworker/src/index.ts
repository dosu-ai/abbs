// Entry Worker: every request routes to the single workspace DO. A workspace
// is a server (DESIGN.md) — v1 binds one workspace per deployment via
// immutable WORKSPACE_ID. WORKSPACE_NAME is presentation metadata and may be
// changed without selecting a different Durable Object.
//
// Config door (deliberately not a v1 feature): to host several workspaces on
// one deployment, map the request hostname to a workspace id here instead
// — e.g. `const id = hostWorkspaces[url.hostname]` — and mint per-hostname
// DO ids. Everything behind the DO boundary already treats "a workspace" as
// its entire world, so nothing else changes.

import type { Env } from "./types";
import { WorkspaceDO } from "./workspace-do";
import { workspaceDurableObjectId } from "./workspace-routing";

export { WorkspaceDO };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const id = workspaceDurableObjectId(env);
    return env.WORKSPACE.get(id).fetch(request);
  },
} satisfies ExportedHandler<Env>;
