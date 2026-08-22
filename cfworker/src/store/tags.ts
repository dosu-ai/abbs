// Port of internal/store/tags.go — tag mutation on threads, discovery
// listing, and per-user tag subscriptions.

import type { TagInfo, Thread } from "../types";
import { Store, StoreErr, advanceReadCursor, currentSeq, getThread, insertEvent, isoNow, seqToken } from "./store";

// updateThreadTags replaces a thread's tag set. Permitted for the creator
// and participants — in a DM, the fixed participant set (visibility already
// guarantees membership); in a public thread, the creator or anyone who has
// posted. Advances the activity cursor and emits thread.tags_changed. Tags
// must already be normalized.
export function updateThreadTags(s: Store, threadId: string, actor: string, tags: string[], atMs: number): Thread {
  const thread = s.tx(() => {
    const t = getThread(s, threadId, actor);
    if (t.kind === "public" && t.creator !== actor) {
      const posted = s.sql
        .exec(`SELECT 1 FROM messages WHERE thread_id = ? AND author = ? LIMIT 1`, threadId, actor)
        .toArray();
      if (posted.length === 0) throw new StoreErr("forbidden");
    }

    const ts = isoNow(atMs);
    const seq = insertEvent(s, "thread.tags_changed", threadId, ts, {
      thread_id: threadId,
      tags,
      actor,
    });
    s.sql.exec(`UPDATE threads SET tags = ?, last_activity_seq = ? WHERE id = ?`, JSON.stringify(tags), seq, threadId);
    s.sql.exec(`DELETE FROM thread_tags WHERE thread_id = ?`, threadId);
    for (const tag of tags) {
      s.sql.exec(`INSERT INTO thread_tags (thread_id, tag) VALUES (?, ?)`, threadId, tag);
    }
    advanceReadCursor(s, actor, threadId, seq);

    t.tags = tags;
    t.last_activity_seq = seqToken(seq);
    return t;
  });
  s.notify();
  return thread;
}

// listTags pages through tags on at least one thread visible to the viewer,
// with usage counts, alphabetically. after is the page anchor (last tag of
// the previous page; empty for the first).
export function listTags(
  s: Store,
  viewer: string,
  after: string,
  limit: number,
): { items: TagInfo[]; nextPage: string | null; asOf: string } {
  const asOf = seqToken(currentSeq(s));
  let items = s.sql
    .exec(
      `SELECT tt.tag, COUNT(*) AS n FROM thread_tags tt JOIN threads t ON t.id = tt.thread_id
	 WHERE tt.tag > ?
	   AND (t.kind = 'public' OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = t.id AND p.username = ?))
	 GROUP BY tt.tag ORDER BY tt.tag LIMIT ?`,
      after,
      viewer,
      limit + 1,
    )
    .toArray()
    .map((r): TagInfo => ({ name: r.tag as string, thread_count: r.n as number }));
  let nextPage: string | null = null;
  if (items.length > limit) {
    items = items.slice(0, limit);
    nextPage = items[limit - 1].name;
  }
  return { items, nextPage, asOf };
}

// subscribeTag is idempotent; the tag need not be in use yet. Subscriptions
// are per-user state, not workspace events.
export function subscribeTag(s: Store, username: string, tag: string): void {
  s.sql.exec(`INSERT OR IGNORE INTO tag_subscriptions (username, tag) VALUES (?, ?)`, username, tag);
}

export function unsubscribeTag(s: Store, username: string, tag: string): void {
  s.sql.exec(`DELETE FROM tag_subscriptions WHERE username = ? AND tag = ?`, username, tag);
}

export function listTagSubscriptions(
  s: Store,
  username: string,
  after: string,
  limit: number,
): { items: string[]; nextPage: string | null; asOf: string } {
  const asOf = seqToken(currentSeq(s));
  let items = s.sql
    .exec(
      `SELECT tag FROM tag_subscriptions WHERE username = ? AND tag > ? ORDER BY tag LIMIT ?`,
      username,
      after,
      limit + 1,
    )
    .toArray()
    .map((r) => r.tag as string);
  let nextPage: string | null = null;
  if (items.length > limit) {
    items = items.slice(0, limit);
    nextPage = items[limit - 1];
  }
  return { items, nextPage, asOf };
}
