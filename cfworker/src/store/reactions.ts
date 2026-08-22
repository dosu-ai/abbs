// Port of internal/store/reactions.go — reaction current-state rows plus
// their event-log entries. Reaction events consume the global sequence but
// deliberately never touch the thread's activity cursor (DESIGN.md).

import type { Reaction } from "../types";
import {
  MESSAGE_COLS,
  Store,
  StoreErr,
  authenticatedViewer,
  currentSeq,
  getThread,
  insertEvent,
  isoNow,
  rowToMessage,
  seqToken,
} from "./store";

function scanMessageRow(s: Store, messageId: string) {
  const rows = s.sql.exec(`SELECT ${MESSAGE_COLS} FROM messages WHERE id = ?`, messageId).toArray();
  if (rows.length === 0) throw new StoreErr("not-found");
  return rowToMessage(rows[0]).msg;
}

// addReaction records (viewer, message, emoji) idempotently: re-adding an
// existing reaction succeeds without a new event. At most 10 distinct emoji
// per user per message; tombstones reject new reactions. The emoji must
// already be normalized (normalizeEmoji).
export function addReaction(s: Store, messageId: string, viewer: string, emojiKey: string, atMs: number): void {
  let emitted = false;
  s.tx(() => {
    const m = scanMessageRow(s, messageId);
    getThread(s, m.thread_id, authenticatedViewer(viewer));
    if (m.deleted) throw new StoreErr("message-deleted");

    const existing = s.sql
      .exec(`SELECT 1 FROM reactions WHERE message_id = ? AND username = ? AND emoji = ?`, messageId, viewer, emojiKey)
      .toArray();
    if (existing.length > 0) return; // idempotent: already present, no event

    const distinct = s.sql
      .exec(`SELECT COUNT(*) AS n FROM reactions WHERE message_id = ? AND username = ?`, messageId, viewer)
      .one().n as number;
    if (distinct >= 10) throw new StoreErr("reaction-limit");

    const ts = isoNow(atMs);
    const seq = insertEvent(s, "reaction.added", m.thread_id, ts, {
      thread_id: m.thread_id,
      message_id: messageId,
      emoji: emojiKey,
      username: viewer,
    });
    s.sql.exec(
      `INSERT INTO reactions (message_id, username, emoji, created_at, seq) VALUES (?, ?, ?, ?, ?)`,
      messageId,
      viewer,
      emojiKey,
      ts,
      seq,
    );
    emitted = true;
  });
  if (emitted) s.notify();
}

// removeReaction removes the viewer's own reaction, idempotently: removing
// an absent reaction succeeds without a new event. Reactions on tombstones
// survive but may still be removed by their reactor.
export function removeReaction(s: Store, messageId: string, viewer: string, emojiKey: string, atMs: number): void {
  let emitted = false;
  s.tx(() => {
    const m = scanMessageRow(s, messageId);
    getThread(s, m.thread_id, authenticatedViewer(viewer));

    const res = s.sql.exec(
      `DELETE FROM reactions WHERE message_id = ? AND username = ? AND emoji = ?`,
      messageId,
      viewer,
      emojiKey,
    );
    if (res.rowsWritten === 0) return; // idempotent: nothing to remove, no event
    insertEvent(s, "reaction.removed", m.thread_id, isoNow(atMs), {
      thread_id: m.thread_id,
      message_id: messageId,
      emoji: emojiKey,
      username: viewer,
    });
    emitted = true;
  });
  if (emitted) s.notify();
}

// listReactions pages through who-reacted-what in creation order; after is
// the seq page anchor.
export function listReactions(
  s: Store,
  messageId: string,
  viewer: string,
  after: number,
  limit: number,
): { items: Reaction[]; nextPage: string | null; asOf: string } {
  const m = scanMessageRow(s, messageId);
  getThread(s, m.thread_id, authenticatedViewer(viewer));
  const asOf = seqToken(currentSeq(s));

  const rows = s.sql
    .exec(
      `SELECT emoji, username, created_at, seq FROM reactions WHERE message_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
      messageId,
      after,
      limit + 1,
    )
    .toArray();
  let items: Reaction[] = [];
  const seqs: number[] = [];
  for (const r of rows) {
    items.push({ emoji: r.emoji as string, username: r.username as string, created_at: r.created_at as string });
    seqs.push(r.seq as number);
  }
  let nextPage: string | null = null;
  if (items.length > limit) {
    items = items.slice(0, limit);
    nextPage = seqToken(seqs[limit - 1]);
  }
  return { items, nextPage, asOf };
}
