// Inbox and per-thread read cursors (port of the inbox/read-cursor handlers
// in server.go).

import type { ReqCtx } from "../context";
import { ProblemError, jsonResponse, noContent } from "../problems";
import { getReadCursor, inbox, seqToken, setReadCursor } from "../store/store";
import { parseSeq } from "../text";
import { authenticate, decodeJSON, notFound, parseLimit, parsePageAnchor } from "./helpers";

export function handleInbox(c: ReqCtx): Response {
  const user = authenticate(c);
  const before = parsePageAnchor(c);
  const limit = parseLimit(c, 50);
  const { items, nextPage, asOf } = inbox(c.store, user.username, before, limit);
  return jsonResponse(200, { items, next_page: nextPage, as_of: asOf });
}

export function handleGetReadCursor(c: ReqCtx): Response {
  const user = authenticate(c);
  try {
    const seq = getReadCursor(c.store, c.params.thread_id, user.username);
    return jsonResponse(200, { seq: seq === null ? null : seqToken(seq) });
  } catch (err) {
    notFound(err, "no such thread");
  }
}

export function handleSetReadCursor(c: ReqCtx): Response {
  const user = authenticate(c);
  const req = decodeJSON(c);
  if (typeof req.seq !== "string") {
    throw new ProblemError(400, "validation", "invalid JSON body: seq must be a string");
  }
  const seq = parseSeq(req.seq);
  if (seq === null) {
    throw new ProblemError(400, "validation", "invalid seq");
  }
  try {
    setReadCursor(c.store, c.params.thread_id, user.username, seq);
    return noContent();
  } catch (err) {
    notFound(err, "no such thread");
  }
}
