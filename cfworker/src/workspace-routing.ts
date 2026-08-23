import { parseWorkspaceConfig } from "./config";
import type { Env } from "./types";

export function workspaceDurableObjectId(env: Env): DurableObjectId {
  const cfg = parseWorkspaceConfig(env);
  return env.WORKSPACE.idFromName(cfg.id);
}
