// GET /v1/events — the catch-up read and the long-poll: the same query,
// differing only in whether the server waits (port of handleEvents in
// server.go). Parked waiters await plain promises, so DO input gates stay
// open — other requests proceed while polls are parked.

import type { ReqCtx } from "../context";
import { ProblemError, jsonResponse } from "../problems";
import { events, type EventFilter } from "../store/store";
import { countCodePoints, normalizeTags, parseIntStrict, parseSeq } from "../text";
import { authenticate } from "./helpers";

export interface ParsedEventQuery {
  after: number;
  filter: EventFilter;
}

function parseEventFilterBool(q: URLSearchParams, name: string): boolean {
  const values = q.getAll(name);
  if (values.length === 0) return false;
  if (values.length !== 1 || (values[0] !== "true" && values[0] !== "false")) {
    throw new ProblemError(400, "validation", `${name} must be true or false`);
  }
  return values[0] === "true";
}

// parseEventQuery owns the cursor and filter parsing shared by long-poll and
// WebSocket transports, so their validation and filtering cannot drift.
export function parseEventQuery(c: ReqCtx): ParsedEventQuery {
  const q = c.url.searchParams;
  const cursors = q.getAll("cursor");
  let after = 0;
  if (cursors.length > 0) {
    if (cursors.length !== 1 || cursors[0] === "") {
      throw new ProblemError(400, "validation", "invalid cursor");
    }
    const n = parseSeq(cursors[0]);
    if (n === null) throw new ProblemError(400, "validation", "invalid cursor");
    after = n;
  }

  const rawTags = q.getAll("tag");
  if (rawTags.length > c.limits.thread_max_tags) {
    throw new ProblemError(400, "validation", `at most ${c.limits.thread_max_tags} event tag filters`);
  }
  for (const rawTag of rawTags) {
    const tag = rawTag.trim().toLowerCase();
    if (tag === "") {
      throw new ProblemError(400, "validation", "event tag filters must not be empty");
    }
    if (countCodePoints(tag) > c.limits.tag_max_chars) {
      throw new ProblemError(400, "validation", `tag ${JSON.stringify(tag)} over ${c.limits.tag_max_chars} characters`);
    }
  }

  return {
    after,
    filter: {
      mentions: parseEventFilterBool(q, "mentions"),
      dms: parseEventFilterBool(q, "dms"),
      subscribedTags: parseEventFilterBool(q, "subscribed_tags"),
      tags: normalizeTags(rawTags),
    },
  };
}

// raceWakeup resolves on the wakeup promise, the timeout, or client abort —
// whichever comes first — and always clears the timer so a woken poll does
// not pin the DO for the rest of its timeout.
function raceWakeup(wakeup: Promise<void>, ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise<void>((resolve) => {
    let settled = false;
    const done = () => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    wakeup.then(done);
    signal?.addEventListener("abort", done);
  });
}

export async function handleEvents(c: ReqCtx): Promise<Response> {
  const user = authenticate(c);
  const query = parseEventQuery(c);
  const q = c.url.searchParams;
  let after = query.after;
  let timeout = 0;
  const timeoutParam = q.get("timeout");
  if (timeoutParam !== null && timeoutParam !== "") {
    const n = parseIntStrict(timeoutParam);
    if (n === null || n < 0 || n > c.limits.poll_max_timeout_seconds) {
      throw new ProblemError(400, "validation", `timeout must be 0..${c.limits.poll_max_timeout_seconds} seconds`);
    }
    timeout = n;
  }
  let limit = 100;
  const limitParam = q.get("limit");
  if (limitParam !== null && limitParam !== "") {
    const n = parseIntStrict(limitParam);
    if (n === null || n < 1 || n > c.limits.events_max_batch) {
      throw new ProblemError(400, "validation", `limit must be 1..${c.limits.events_max_batch}`);
    }
    limit = n;
  }
  const deadline = Date.now() + timeout * 1000;
  for (;;) {
    // Subscribe before querying: an append between the query and the wait
    // still wakes us, so no event can slip through.
    const waiter = c.waitForEvent();
    try {
      const batch = events(c.store, user.username, after, limit, query.filter);
      const remaining = deadline - Date.now();
      if (batch.events.length > 0 || remaining <= 0) {
        // Empty batch echoes the request cursor — the client loop is dumb
        // and safe.
        return jsonResponse(200, { events: batch.events, cursor: batch.cursor });
      }
      c.hooks?.afterEventsQuery?.();
      await raceWakeup(waiter.promise, remaining, c.request.signal ?? undefined);
      if (c.request.signal?.aborted) {
        // The client is gone; the response is never delivered.
        return jsonResponse(200, { events: [], cursor: batch.cursor });
      }
    } finally {
      waiter.cancel();
    }
  }
}
