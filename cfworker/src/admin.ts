// The operator trust plane. The Go principle: admin is granted out-of-band
// on the operator's plane (direct DB-file access via `abbs admin …`), never
// over /v1. The Cloudflare equivalent of file access is deploy-time secrets:
//
// 1. Seeded bootstrap admin — api-key mode with ADMIN_BOOTSTRAP_TOKEN set
//    idempotently ensures ADMIN_USERNAME exists with admin=1 and
//    token_hash = sha256(secret). Rotating the secret + redeploy rotates the
//    credential.
// 2. /admin/* operator endpoints — day-2 parity with
//    `abbs admin create-user|grant|revoke|rotate-key`. Gated by
//    OPERATOR_TOKEN (constant-time compare), disabled entirely when the
//    secret is unset, and deliberately outside /spec — an implementation
//    detail exactly like the Go CLI. The conformance suite never touches
//    them.

import type { Env, User } from "./types";
import { AUTH_API_KEY } from "./types";
import { ProblemError, jsonResponse, noContent, problemResponse } from "./problems";
import { bearerToken, mintToken, sha256Hex, timingSafeEqualStr } from "./auth";
import { Store, StoreErr, insertEvent, isoNow } from "./store/store";
import { claimUser, rotateToken, setAdmin } from "./store/users";
import { countCodePoints, usernameRE } from "./text";

// seedBootstrapAdmin runs inside blockConcurrencyWhile at DO init.
export async function seedBootstrapAdmin(store: Store, env: Env): Promise<void> {
  if (env.AUTH_MODE !== AUTH_API_KEY || !env.ADMIN_BOOTSTRAP_TOKEN) return;
  const username = env.ADMIN_USERNAME || "admin";
  const tokenHash = await sha256Hex(env.ADMIN_BOOTSTRAP_TOKEN);
  store.tx(() => {
    const rows = store.sql
      .exec(`SELECT token_hash, admin FROM users WHERE username = ?`, username)
      .toArray();
    if (rows.length === 0) {
      const ts = isoNow(Date.now());
      store.sql.exec(
        `INSERT INTO users (username, kind, created_at, token_hash, admin) VALUES (?, 'human', ?, ?, 1)`,
        username,
        ts,
        tokenHash,
      );
      const user: User = { username, kind: "human", admin: true, deactivated: false, created_at: ts };
      insertEvent(store, "user.created", null, ts, { user });
    } else if (rows[0].token_hash !== tokenHash || (rows[0].admin as number) !== 1) {
      store.sql.exec(`UPDATE users SET token_hash = ?, admin = 1 WHERE username = ?`, tokenHash, username);
    }
  });
}

// handleAdmin serves the /admin/* plane. Mounted before the /v1 router; when
// OPERATOR_TOKEN is unset the plane does not exist (404, indistinguishable
// from any other unknown path).
export async function handleAdmin(
  store: Store,
  env: Env,
  request: Request,
  url: URL,
  bodyText: string,
): Promise<Response> {
  if (!env.OPERATOR_TOKEN) {
    return problemResponse(404, "not-found", "no such endpoint");
  }
  const bearer = bearerToken(request);
  if (bearer === null || !(await timingSafeEqualStr(bearer, env.OPERATOR_TOKEN))) {
    return problemResponse(401, "unauthorized", "invalid operator token");
  }

  const parts = url.pathname.split("/").filter((s) => s !== "");
  try {
    // POST /admin/users — abbs admin create-user
    if (request.method === "POST" && parts.length === 2 && parts[1] === "users") {
      return await handleCreateUser(store, bodyText);
    }
    // POST /admin/users/{username}/(grant|revoke|rotate-key)
    if (request.method === "POST" && parts.length === 4 && parts[1] === "users") {
      const username = decodeURIComponent(parts[2]);
      switch (parts[3]) {
        case "grant":
          setAdmin(store, username, true);
          return noContent();
        case "revoke":
          setAdmin(store, username, false);
          return noContent();
        case "rotate-key": {
          const { token, tokenHash } = await mintToken();
          rotateToken(store, username, tokenHash);
          return jsonResponse(200, { token });
        }
      }
    }
    return problemResponse(404, "not-found", "no such endpoint");
  } catch (err) {
    if (err instanceof ProblemError) return err.response();
    if (err instanceof StoreErr && err.code === "not-found") {
      return problemResponse(404, "not-found", "no such user");
    }
    if (err instanceof StoreErr && err.code === "username-taken") {
      return problemResponse(409, "username-taken", "username already claimed");
    }
    return problemResponse(500, "internal", err instanceof Error ? err.message : String(err));
  }
}

async function handleCreateUser(store: Store, bodyText: string): Promise<Response> {
  let req: Record<string, unknown>;
  try {
    req = JSON.parse(bodyText) as Record<string, unknown>;
  } catch (err) {
    return problemResponse(400, "validation", "invalid JSON body: " + (err instanceof Error ? err.message : String(err)));
  }
  const username = req.username;
  if (typeof username !== "string" || !usernameRE.test(username)) {
    return problemResponse(400, "validation", "username must match ^[a-z0-9][a-z0-9._-]{0,31}$");
  }
  const kind = req.kind ?? "agent";
  if (kind !== "human" && kind !== "agent") {
    return problemResponse(400, "validation", `kind must be "human" or "agent"`);
  }
  let displayName: string | null = null;
  if (req.display_name !== undefined && req.display_name !== null) {
    if (typeof req.display_name !== "string" || countCodePoints(req.display_name) > 100) {
      return problemResponse(400, "validation", "display_name must be a string of at most 100 characters");
    }
    displayName = req.display_name;
  }
  const { token, tokenHash } = await mintToken();
  const user = claimUser(store, username, kind, displayName, tokenHash, Date.now());
  if (req.admin === true) {
    setAdmin(store, username, true);
    user.admin = true;
  }
  return jsonResponse(201, { user, token });
}
