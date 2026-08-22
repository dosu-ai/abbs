// Thread creation, retrieval, and listing (port of the thread handlers in
// server.go).

import type { ReqCtx } from "../context";
import { ProblemError, jsonResponse } from "../problems";
import { createThread, getThread, listThreads } from "../store/store";
import { countCodePoints, normalizeTags, parseSeq, usernameRE } from "../text";
import {
  authenticate,
  checkContent,
  conditionalReadViewer,
  decodeJSON,
  notFound,
  parseLimit,
  parsePageAnchor,
  requireString,
} from "./helpers";

export function handleCreateThread(c: ReqCtx): Response {
  const user = authenticate(c);
  const req = decodeJSON(c);
  const title = requireString(req.title ?? "", "title");
  const titleLen = countCodePoints(title);
  if (titleLen === 0 || titleLen > c.limits.title_max_chars) {
    throw new ProblemError(400, "validation", `title must be 1..${c.limits.title_max_chars} characters`);
  }
  const content = checkContent(c, req.content ?? "");
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

  // Presence of participants makes the thread a DM; the set is participants
  // ∪ creator, deduplicated, and is fixed forever.
  let participants: string[] = [];
  if (req.participants !== undefined && req.participants !== null) {
    if (!Array.isArray(req.participants) || req.participants.some((p) => typeof p !== "string")) {
      throw new ProblemError(400, "validation", "invalid JSON body: participants must be an array of strings");
    }
    const set = new Set<string>([user.username]);
    participants = [user.username];
    for (const p of req.participants as string[]) {
      if (!usernameRE.test(p)) {
        throw new ProblemError(400, "validation", `invalid participant username "${p}"`);
      }
      if (!set.has(p)) {
        set.add(p);
        participants.push(p);
      }
    }
    if (participants.length < 2) {
      throw new ProblemError(400, "validation", "a DM needs at least one participant besides the creator");
    }
    if (participants.length > c.limits.dm_max_participants) {
      throw new ProblemError(
        400,
        "validation",
        `at most ${c.limits.dm_max_participants} DM participants including the creator`,
      );
    }
  }

  const { thread } = createThread(c.store, user.username, title, content, tags, participants, Date.now());
  return jsonResponse(201, thread);
}

export function handleGetThread(c: ReqCtx): Response {
  const { viewer } = conditionalReadViewer(c);
  try {
    return jsonResponse(200, getThread(c.store, c.params.thread_id, viewer));
  } catch (err) {
    notFound(err, "no such thread");
  }
}

export function handleListThreads(c: ReqCtx): Response {
  const { viewer } = conditionalReadViewer(c);
  const q = c.url.searchParams;
  let since = 0;
  const sinceParam = q.get("since");
  if (sinceParam !== null && sinceParam !== "") {
    const n = parseSeq(sinceParam);
    if (n === null) {
      throw new ProblemError(400, "validation", "invalid since cursor");
    }
    since = n;
  }
  const before = parsePageAnchor(c);
  const limit = parseLimit(c, 50);
  const tags = normalizeTags(q.getAll("tag"));
  if (tags.length > c.limits.thread_max_tags) {
    throw new ProblemError(400, "validation", `at most ${c.limits.thread_max_tags} tag filters`);
  }
  for (const tag of tags) {
    if (countCodePoints(tag) > c.limits.tag_max_chars) {
      throw new ProblemError(400, "validation", `tag "${tag}" over ${c.limits.tag_max_chars} characters`);
    }
  }
  const { items, nextPage, asOf } = listThreads(c.store, viewer, since, before, tags, limit);
  return jsonResponse(200, { items, next_page: nextPage, as_of: asOf });
}
