// Message posting (with the reply-loop guard), listing, reads, edits, and
// tombstone deletes (port of the message handlers in server.go and
// handlers_full.go).

import type { ReqCtx } from "../context";
import { ProblemError, jsonResponse } from "../problems";
import { loopGuardTrips, retryAfterSeconds } from "../loopguard";
import { deleteMessage, editMessage, getMessage, lastAuthors, listMessages, postMessage } from "../store/messages";
import {
  authenticate,
  checkContent,
  conditionalReadViewer,
  decodeJSON,
  notFound,
  parseLimit,
  parsePageAnchor,
} from "./helpers";

export function handlePostMessage(c: ReqCtx): Response {
  const user = authenticate(c);
  const req = decodeJSON(c);
  const content = checkContent(c, req.content ?? "");
  const threadId = c.params.thread_id;

  // Reply-loop guard: the last N messages plus this one, authored by ≤2
  // distinct users, inside the window — the runaway agent-pair shape. Rapid
  // legitimate dialogs are distinguished by pace, not by content.
  const { authors, oldestMs } = lastAuthors(c.store, threadId, c.cfg.loopGuard.messages);
  if (loopGuardTrips(authors, oldestMs, user.username, Date.now(), c.cfg.loopGuard)) {
    throw new ProblemError(
      429,
      "loop-guard",
      "reply-loop guard: too many rapid messages between too few authors in this thread",
      { "Retry-After": String(retryAfterSeconds(c.cfg.loopGuard)) },
    );
  }

  try {
    return jsonResponse(201, postMessage(c.store, threadId, user.username, content, Date.now()));
  } catch (err) {
    notFound(err, "no such thread");
  }
}

export function handleListMessages(c: ReqCtx): Response {
  const { viewer } = conditionalReadViewer(c);
  const after = parsePageAnchor(c);
  const limit = parseLimit(c, 50);
  try {
    const { items, nextPage, asOf } = listMessages(c.store, c.params.thread_id, viewer, after, limit);
    return jsonResponse(200, { items, next_page: nextPage, as_of: asOf });
  } catch (err) {
    notFound(err, "no such thread");
  }
}

export function handleGetMessage(c: ReqCtx): Response {
  const user = authenticate(c);
  return jsonResponse(200, getMessage(c.store, c.params.message_id, user.username));
}

export function handleEditMessage(c: ReqCtx): Response {
  const user = authenticate(c);
  const req = decodeJSON(c);
  const content = checkContent(c, req.content ?? "");
  return jsonResponse(200, editMessage(c.store, c.params.message_id, user.username, content, Date.now()));
}

export function handleDeleteMessage(c: ReqCtx): Response {
  const user = authenticate(c);
  return jsonResponse(200, deleteMessage(c.store, c.params.message_id, user.username, user.admin, Date.now()));
}
