// Port of internal/store/messages.go (plus PostMessage/ListMessages from
// store.go): message reads, appends, in-place edits, and tombstone deletes.

import type { Message } from "../types";
import { extractMentions } from "../mentions";
import {
  MESSAGE_COLS,
  Store,
  StoreErr,
  advanceReadCursor,
  currentSeq,
  getThread,
  insertEvent,
  insertMentions,
  isoNow,
  newId,
  rowToMessage,
  seqToken,
  tallies,
  updateEventPayload,
  userExists,
} from "./store";

function scanMessage(s: Store, id: string): { msg: Message; createdSeq: number } {
  const rows = s.sql.exec(`SELECT ${MESSAGE_COLS} FROM messages WHERE id = ?`, id).toArray();
  if (rows.length === 0) throw new StoreErr("not-found");
  return rowToMessage(rows[0]);
}

// getMessage returns a message (tombstones included) if its thread is
// visible to the viewer.
export function getMessage(s: Store, id: string, viewer: string): Message {
  const { msg } = scanMessage(s, id);
  getThread(s, msg.thread_id, viewer);
  msg.reactions = tallies(s, msg.id);
  return msg;
}

// postMessage appends a message to a thread the author can see, advancing
// the thread's activity cursor.
export function postMessage(s: Store, threadId: string, author: string, content: string, atMs: number): Message {
  const msg = s.tx(() => {
    getThread(s, threadId, author);

    const ts = isoNow(atMs);
    const seq = insertEvent(s, "message.created", threadId, ts, null);
    const mentioned = extractMentions(content, (u) => userExists(s, u));
    const m: Message = {
      id: newId(atMs),
      thread_id: threadId,
      author,
      content,
      ...(mentioned.length > 0 ? { mentions: mentioned } : {}),
      deleted: false,
      created_at: ts,
      seq: seqToken(seq),
      reactions: [],
    };
    updateEventPayload(s, seq, { message: m });
    s.sql.exec(
      `INSERT INTO messages (id, thread_id, author, content, mentions, created_at, created_seq, seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
      m.id,
      threadId,
      author,
      content,
      JSON.stringify(mentioned),
      ts,
      seq,
      seq,
    );
    insertMentions(s, m.id, threadId, mentioned, seq);
    s.sql.exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ?`, seq, threadId);
    advanceReadCursor(s, author, threadId, seq);
    return m;
  });
  s.notify();
  return msg;
}

// listMessages pages through a thread's messages in creation order. after is
// the created_seq page anchor (0 = from the start); nextPage is the anchor
// for the following page, null on the last one.
export function listMessages(
  s: Store,
  threadId: string,
  viewer: string,
  after: number,
  limit: number,
): { items: Message[]; nextPage: string | null; asOf: string } {
  getThread(s, threadId, viewer);
  const asOf = seqToken(currentSeq(s));

  const rows = s.sql
    .exec(
      `SELECT ${MESSAGE_COLS} FROM messages WHERE thread_id = ? AND created_seq > ? ORDER BY created_seq LIMIT ?`,
      threadId,
      after,
      limit + 1,
    )
    .toArray();

  let scanned = rows.map(rowToMessage);
  let nextPage: string | null = null;
  if (scanned.length > limit) {
    // The extra row only proved another page exists; the anchor is the
    // created_seq of the last item kept (stable across edits, unlike seq).
    scanned = scanned.slice(0, limit);
    nextPage = seqToken(scanned[limit - 1].createdSeq);
  }
  const items = scanned.map(({ msg }) => {
    msg.reactions = tallies(s, msg.id);
    return msg;
  });
  return { items, nextPage, asOf };
}

// editMessage replaces a message's content in place. Author-only; message
// IDs are stable across edits; the edit advances the thread's activity
// cursor and re-extracts mentions (a mention added by an edit reaches its
// target's inbox at the edit's seq).
export function editMessage(s: Store, id: string, author: string, content: string, atMs: number): Message {
  const msg = s.tx(() => {
    const { msg: m } = scanMessage(s, id);
    getThread(s, m.thread_id, author);
    if (m.deleted) throw new StoreErr("message-deleted");
    if (m.author !== author) throw new StoreErr("forbidden");

    const ts = isoNow(atMs);
    const seq = insertEvent(s, "message.edited", m.thread_id, ts, null);
    const mentioned = extractMentions(content, (u) => userExists(s, u));
    s.sql.exec(`DELETE FROM mentions WHERE message_id = ?`, id);
    insertMentions(s, id, m.thread_id, mentioned, seq);
    s.sql.exec(
      `UPDATE messages SET content = ?, mentions = ?, edited_at = ?, seq = ? WHERE id = ?`,
      content,
      JSON.stringify(mentioned),
      ts,
      seq,
      id,
    );
    s.sql.exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ?`, seq, m.thread_id);
    advanceReadCursor(s, author, m.thread_id, seq);

    m.content = content;
    if (mentioned.length > 0) m.mentions = mentioned;
    else delete m.mentions;
    m.edited_at = ts;
    m.seq = seqToken(seq);
    m.reactions = tallies(s, id);
    updateEventPayload(s, seq, { message: m });
    return m;
  });
  s.notify();
  return msg;
}

// deleteMessage tombstones a message: id and position survive, content and
// mentions go. Author or admin only; deleted_by records who, so moderation
// is distinguishable from retraction. Idempotent — deleting a tombstone
// returns it unchanged without a new event. Admins may delete messages in
// threads they cannot read (moderation by id).
export function deleteMessage(s: Store, id: string, actor: string, isAdmin: boolean, atMs: number): Message {
  let emitted = false;
  const msg = s.tx(() => {
    const { msg: m } = scanMessage(s, id);
    if (!isAdmin) {
      getThread(s, m.thread_id, actor);
      if (m.author !== actor) throw new StoreErr("forbidden");
    }
    m.reactions = tallies(s, id);
    if (m.deleted) return m;

    const ts = isoNow(atMs);
    const seq = insertEvent(s, "message.deleted", m.thread_id, ts, null);
    s.sql.exec(`DELETE FROM mentions WHERE message_id = ?`, id);
    s.sql.exec(
      `UPDATE messages SET content = NULL, mentions = NULL, deleted = 1, deleted_at = ?, deleted_by = ?, seq = ? WHERE id = ?`,
      ts,
      actor,
      seq,
      id,
    );
    s.sql.exec(`UPDATE threads SET last_activity_seq = ? WHERE id = ?`, seq, m.thread_id);

    m.deleted = true;
    delete m.content;
    delete m.mentions;
    m.deleted_at = ts;
    m.deleted_by = actor;
    m.seq = seqToken(seq);
    updateEventPayload(s, seq, { message: m });
    emitted = true;
    return m;
  });
  if (emitted) s.notify();
  return msg;
}

// lastAuthors returns the authors of a thread's most recent n messages,
// newest first, with the created_at (ms) of the oldest returned message —
// the reply-loop guard's probe.
export function lastAuthors(s: Store, threadId: string, n: number): { authors: string[]; oldestMs: number } {
  const rows = s.sql
    .exec(`SELECT author, created_at FROM messages WHERE thread_id = ? ORDER BY created_seq DESC LIMIT ?`, threadId, n)
    .toArray();
  const authors = rows.map((r) => r.author as string);
  let oldestMs = 0;
  if (rows.length > 0) {
    oldestMs = Date.parse(rows[rows.length - 1].created_at as string);
  }
  return { authors, oldestMs };
}
