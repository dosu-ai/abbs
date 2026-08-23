// Centralized operator-binding parsing. Both the entry Worker (which chooses
// the Durable Object id from the immutable workspace id) and the Durable
// Object (which serves discovery using the mutable display name and authorizes
// reads) call this exact function, so they cannot silently apply different
// defaults or validation.

import type { Env } from "./types";
import { AUTH_API_KEY, AUTH_FIRST_CLAIM } from "./types";

export const VISIBILITY_PRIVATE = "private";
export const VISIBILITY_PUBLIC = "public";

export interface WorkspaceConfig {
  id: string;
  name: string;
  description?: string;
  visibility: typeof VISIBILITY_PRIVATE | typeof VISIBILITY_PUBLIC;
  canonicalUrl?: string;
  directoryListing: boolean;
  authMode: typeof AUTH_FIRST_CLAIM | typeof AUTH_API_KEY;
}

export function parseWorkspaceConfig(env: Env): WorkspaceConfig {
  const name = env.WORKSPACE_NAME ?? "abbs";
  if (countCodePoints(name) < 1 || countCodePoints(name) > 100) {
    throw new Error("WORKSPACE_NAME must be 1..100 Unicode code points");
  }

  // Backwards compatibility: deployments created before WORKSPACE_ID was
  // introduced keep routing by their existing WORKSPACE_NAME. Production
  // environments should set WORKSPACE_ID explicitly and never change it.
  const id = env.WORKSPACE_ID ?? name;
  if (countCodePoints(id) < 1 || countCodePoints(id) > 100) {
    throw new Error("WORKSPACE_ID must be 1..100 Unicode code points");
  }

  const description = env.WORKSPACE_DESCRIPTION ?? "";
  if (countCodePoints(description) > 1000) {
    throw new Error("WORKSPACE_DESCRIPTION must be at most 1000 Unicode code points");
  }

  const rawVisibility = env.WORKSPACE_VISIBILITY ?? "";
  if (rawVisibility !== "" && rawVisibility !== VISIBILITY_PRIVATE && rawVisibility !== VISIBILITY_PUBLIC) {
    throw new Error(
      `unsupported WORKSPACE_VISIBILITY "${rawVisibility}" (want "${VISIBILITY_PRIVATE}" or "${VISIBILITY_PUBLIC}")`,
    );
  }
  const visibility = rawVisibility === VISIBILITY_PUBLIC ? VISIBILITY_PUBLIC : VISIBILITY_PRIVATE;

  const canonicalUrl = env.WORKSPACE_CANONICAL_URL ?? "";
  if (canonicalUrl !== "") validateCanonicalOrigin(canonicalUrl);
  if (visibility === VISIBILITY_PUBLIC && canonicalUrl === "") {
    throw new Error("public workspace requires WORKSPACE_CANONICAL_URL");
  }

  const rawListing = env.WORKSPACE_DIRECTORY_LISTING ?? "";
  if (rawListing !== "" && rawListing !== "true" && rawListing !== "false") {
    throw new Error('WORKSPACE_DIRECTORY_LISTING must be "true" or "false"');
  }
  const directoryListing = rawListing === "true";
  if (directoryListing && visibility !== VISIBILITY_PUBLIC) {
    throw new Error("directory listing requires public workspace visibility");
  }
  if (directoryListing && description === "") {
    throw new Error("directory listing requires a non-empty workspace description");
  }

  const rawMode = env.AUTH_MODE ?? "";
  if (rawMode !== "" && rawMode !== AUTH_API_KEY && rawMode !== AUTH_FIRST_CLAIM) {
    throw new Error(`unsupported AUTH_MODE "${rawMode}" (want "${AUTH_FIRST_CLAIM}" or "${AUTH_API_KEY}")`);
  }
  const authMode = rawMode === AUTH_API_KEY ? AUTH_API_KEY : AUTH_FIRST_CLAIM;

  return {
    id,
    name,
    ...(description !== "" ? { description } : {}),
    visibility,
    ...(canonicalUrl !== "" ? { canonicalUrl } : {}),
    directoryListing,
    authMode,
  };
}

function validateCanonicalOrigin(raw: string): void {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    throw new Error("WORKSPACE_CANONICAL_URL must be a valid HTTPS origin");
  }
  if (
    raw !== raw.trim() ||
    !/^https:\/\/[^/?#]+\/?$/.test(raw) ||
    u.protocol !== "https:" ||
    u.hostname === "" ||
    u.username !== "" ||
    u.password !== "" ||
    u.pathname !== "/" ||
    u.search !== "" ||
    u.hash !== ""
  ) {
    throw new Error("WORKSPACE_CANONICAL_URL must be an HTTPS origin with no credentials, path, query, or fragment");
  }
}

function countCodePoints(s: string): number {
  return [...s].length;
}
