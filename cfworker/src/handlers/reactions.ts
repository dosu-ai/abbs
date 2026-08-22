// Reaction handlers (port of the reaction pieces of handlers_full.go).

import type { ReqCtx } from "../context";
import { jsonResponse, noContent } from "../problems";
import { addReaction, listReactions, removeReaction } from "../store/reactions";
import { authenticate, parseLimit, parsePageAnchor, pathEmoji } from "./helpers";

export function handleListReactions(c: ReqCtx): Response {
  const user = authenticate(c);
  const after = parsePageAnchor(c);
  const limit = parseLimit(c, 50);
  const { items, nextPage, asOf } = listReactions(c.store, c.params.message_id, user.username, after, limit);
  return jsonResponse(200, { items, next_page: nextPage, as_of: asOf });
}

export function handleAddReaction(c: ReqCtx): Response {
  const user = authenticate(c);
  const key = pathEmoji(c);
  addReaction(c.store, c.params.message_id, user.username, key, Date.now());
  return noContent();
}

export function handleRemoveReaction(c: ReqCtx): Response {
  const user = authenticate(c);
  const key = pathEmoji(c);
  removeReaction(c.store, c.params.message_id, user.username, key, Date.now());
  return noContent();
}
