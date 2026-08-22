// Port of internal/store/users.go plus ClaimUser/UserByTokenHash from
// store.go: principals, credentials, and the operator actions.

import type { User } from "../types";
import { Row, Store, StoreErr, insertEvent, isoNow, seqToken, currentSeq } from "./store";

const USER_COLS = `username, kind, display_name, owned_by, admin, deactivated, created_at`;

export function rowToUser(r: Row): User {
  const u: User = {
    username: r.username as string,
    kind: r.kind as string,
    admin: (r.admin as number) !== 0,
    deactivated: (r.deactivated as number) !== 0,
    created_at: r.created_at as string,
  };
  if (r.display_name !== null) u.display_name = r.display_name as string;
  if (r.owned_by !== null) u.owned_by = r.owned_by as string;
  return u;
}

// claimUser implements first-claim-wins. tokenHash is the SHA-256 hex of the
// bearer token — tokens are stored hashed, introspection is a lookup.
export function claimUser(
  s: Store,
  username: string,
  kind: string,
  displayName: string | null,
  tokenHash: string,
  atMs: number,
): User {
  const user = s.tx(() => {
    const ts = isoNow(atMs);
    try {
      s.sql.exec(
        `INSERT INTO users (username, kind, display_name, created_at, token_hash) VALUES (?, ?, ?, ?, ?)`,
        username,
        kind,
        displayName,
        ts,
        tokenHash,
      );
    } catch (err) {
      if (err instanceof Error && err.message.includes("users.username")) {
        throw new StoreErr("username-taken");
      }
      throw err;
    }
    const u: User = {
      username,
      kind,
      ...(displayName !== null ? { display_name: displayName } : {}),
      admin: false,
      deactivated: false,
      created_at: ts,
    };
    insertEvent(s, "user.created", null, ts, { user: u });
    return u;
  });
  s.notify();
  return user;
}

// userByTokenHash resolves a bearer credential to its principal, or null.
export function userByTokenHash(s: Store, tokenHash: string): User | null {
  const rows = s.sql.exec(`SELECT ${USER_COLS} FROM users WHERE token_hash = ?`, tokenHash).toArray();
  return rows.length === 0 ? null : rowToUser(rows[0]);
}

export function getUser(s: Store, username: string): User {
  const rows = s.sql.exec(`SELECT ${USER_COLS} FROM users WHERE username = ?`, username).toArray();
  if (rows.length === 0) throw new StoreErr("not-found");
  return rowToUser(rows[0]);
}

// listUsers pages through all users alphabetically; after is the page anchor
// (last username of the previous page; empty for the first).
export function listUsers(
  s: Store,
  after: string,
  limit: number,
): { items: User[]; nextPage: string | null; asOf: string } {
  const asOf = seqToken(currentSeq(s));
  let items = s.sql
    .exec(`SELECT ${USER_COLS} FROM users WHERE username > ? ORDER BY username LIMIT ?`, after, limit + 1)
    .toArray()
    .map(rowToUser);
  let nextPage: string | null = null;
  if (items.length > limit) {
    items = items.slice(0, limit);
    nextPage = items[limit - 1].username;
  }
  return { items, nextPage, asOf };
}

// deactivateUser kills a user's credentials while keeping their records and
// attribution. Idempotent: deactivating a deactivated user returns the
// current state without a new event.
export function deactivateUser(s: Store, username: string, atMs: number): User {
  let emitted = false;
  const user = s.tx(() => {
    const u = getUser(s, username);
    if (u.deactivated) return u;
    s.sql.exec(`UPDATE users SET deactivated = 1 WHERE username = ?`, username);
    insertEvent(s, "user.deactivated", null, isoNow(atMs), { username });
    u.deactivated = true;
    emitted = true;
    return u;
  });
  if (emitted) s.notify();
  return user;
}

// rotateToken replaces a user's credential with a new one, revoking the old
// immediately (introspection is a lookup on the stored hash). A credential
// operation, not workspace activity — no event is emitted.
export function rotateToken(s: Store, username: string, tokenHash: string): void {
  s.tx(() => {
    getUser(s, username); // not-found when missing
    s.sql.exec(`UPDATE users SET token_hash = ? WHERE username = ?`, tokenHash, username);
  });
}

// setAdmin grants or revokes the admin role — an operator action,
// deliberately not part of /v1 and orthogonal to auth mode.
export function setAdmin(s: Store, username: string, admin: boolean): void {
  s.tx(() => {
    getUser(s, username); // not-found when missing
    s.sql.exec(`UPDATE users SET admin = ? WHERE username = ?`, admin ? 1 : 0, username);
  });
}
