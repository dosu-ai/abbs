// Tag mutation, discovery, and subscription handlers (port of the tag
// pieces of handlers_full.go).

import type { ReqCtx } from "../context";
import { ProblemError, jsonResponse, noContent } from "../problems";
import { listTags, listTagSubscriptions, subscribeTag, unsubscribeTag, updateThreadTags } from "../store/tags";
import { countCodePoints, normalizeTags } from "../text";
import { authenticate, decodeJSON, parseLimit, pathTag } from "./helpers";

export function handleUpdateThreadTags(c: ReqCtx): Response {
  const user = authenticate(c);
  const req = decodeJSON(c);
  const rawTags =
    req.tags === undefined || req.tags === null
      ? []
      : Array.isArray(req.tags) && req.tags.every((t) => typeof t === "string")
        ? (req.tags as string[])
        : null;
  if (rawTags === null) {
    throw new ProblemError(400, "validation", "invalid JSON body: tags must be an array of strings");
  }
  const tags = normalizeTags(rawTags);
  if (tags.length > c.limits.thread_max_tags) {
    throw new ProblemError(400, "validation", `at most ${c.limits.thread_max_tags} tags per thread`);
  }
  for (const t of tags) {
    if (countCodePoints(t) > c.limits.tag_max_chars) {
      throw new ProblemError(400, "validation", `tag "${t}" over ${c.limits.tag_max_chars} characters`);
    }
  }
  return jsonResponse(200, updateThreadTags(c.store, c.params.thread_id, user.username, tags, Date.now()));
}

export function handleListTags(c: ReqCtx): Response {
  const user = authenticate(c);
  const limit = parseLimit(c, 50);
  const after = c.url.searchParams.get("page") ?? "";
  const { items, nextPage, asOf } = listTags(c.store, user.username, after, limit);
  return jsonResponse(200, { items, next_page: nextPage, as_of: asOf });
}

export function handleSubscribeTag(c: ReqCtx): Response {
  const user = authenticate(c);
  const tag = pathTag(c);
  subscribeTag(c.store, user.username, tag);
  return noContent();
}

export function handleUnsubscribeTag(c: ReqCtx): Response {
  const user = authenticate(c);
  const tag = pathTag(c);
  unsubscribeTag(c.store, user.username, tag);
  return noContent();
}

export function handleListTagSubscriptions(c: ReqCtx): Response {
  const user = authenticate(c);
  const limit = parseLimit(c, 50);
  const after = c.url.searchParams.get("page") ?? "";
  const { items, nextPage, asOf } = listTagSubscriptions(c.store, user.username, after, limit);
  return jsonResponse(200, { items, next_page: nextPage, as_of: asOf });
}
