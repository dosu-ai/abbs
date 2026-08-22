// Users surface (port of handleClaimUser in server.go and the user handlers
// in handlers_full.go).

import type { ReqCtx } from "../context";
import { ProblemError, jsonResponse } from "../problems";
import { AUTH_FIRST_CLAIM } from "../types";
import { StoreErr } from "../store/store";
import { claimUser, deactivateUser, getUser, listUsers } from "../store/users";
import { countCodePoints, usernameRE } from "../text";
import { authenticate, decodeJSON, parseLimit } from "./helpers";

export function handleClaimUser(c: ReqCtx): Response {
  // The endpoint is the credential ceremony for both modes: first-claim lets
  // anyone claim an unclaimed name; api-key mode turns it into admin-issued
  // key provisioning and rejects everyone else.
  if (c.cfg.authMode !== AUTH_FIRST_CLAIM) {
    const actor = authenticate(c);
    if (!actor.admin) {
      throw new ProblemError(
        403,
        "forbidden",
        "first-claim is disabled on this server; user creation requires an admin API key",
      );
    }
  }
  const req = decodeJSON(c);
  const username = req.username;
  if (typeof username !== "string" || !usernameRE.test(username)) {
    throw new ProblemError(400, "validation", "username must match ^[a-z0-9][a-z0-9._-]{0,31}$");
  }
  if (req.kind !== "human" && req.kind !== "agent") {
    throw new ProblemError(400, "validation", `kind must be "human" or "agent"`);
  }
  let displayName: string | null = null;
  if (req.display_name !== undefined && req.display_name !== null) {
    if (typeof req.display_name !== "string") {
      throw new ProblemError(400, "validation", "invalid JSON body: display_name must be a string");
    }
    if (countCodePoints(req.display_name) > 100) {
      throw new ProblemError(400, "validation", "display_name over 100 characters");
    }
    displayName = req.display_name;
  }
  // Minted in the DO's fetch path (crypto.subtle is async; this handler
  // must stay synchronous inside the idempotency transaction).
  if (c.mintedToken === undefined) {
    throw new Error("claim handler invoked without a pre-minted token");
  }
  const { token, tokenHash } = c.mintedToken;
  let user;
  try {
    user = claimUser(c.store, username, req.kind, displayName, tokenHash, Date.now());
  } catch (err) {
    if (err instanceof StoreErr && err.code === "username-taken") {
      throw new ProblemError(409, "username-taken", `"${username}" is already claimed`);
    }
    throw err;
  }
  return jsonResponse(201, { user, token });
}

export function handleListUsers(c: ReqCtx): Response {
  authenticate(c);
  const limit = parseLimit(c, 50);
  const after = c.url.searchParams.get("page") ?? "";
  const { items, nextPage, asOf } = listUsers(c.store, after, limit);
  return jsonResponse(200, { items, next_page: nextPage, as_of: asOf });
}

export function handleGetUser(c: ReqCtx): Response {
  authenticate(c);
  return jsonResponse(200, getUser(c.store, c.params.username));
}

export function handleDeactivateUser(c: ReqCtx): Response {
  const actor = authenticate(c);
  if (!actor.admin) {
    throw new ProblemError(403, "forbidden", "admin role required");
  }
  return jsonResponse(200, deactivateUser(c.store, c.params.username, Date.now()));
}
