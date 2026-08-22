// Port of internal/store/store.go — the SQLite storage core: a single
// append-only events table whose AUTOINCREMENT primary key is the global
// monotonic sequence (that column *is* the cursor), plus current-state
// tables for users, threads, and messages. The Durable Object's
// single-threaded execution and transactionSync give the same "sequence
// order equals commit order" property SQLite's serialized writes give the
// Go server for free.

import type { Event, InboxItem, Message, ReactionTally, Thread } from "../types";
import { extractMentions } from "../mentions";
import { SCHEMA } from "./schema";

export type StoreErrCode = "not-found" | "username-taken" | "forbidden" | "message-deleted" | "reaction-limit";

export class StoreErr extends Error {
  constructor(
    public code: StoreErrCode,
    message?: string,
  ) {
    super(message ?? code);
  }
}

// UnknownParticipantErr reports a DM participant that is not a user.
export class UnknownParticipantErr extends Error {
  constructor(public username: string) {
    super(`unknown participant "${username}"`);
  }
}

export type Row = Record<string, SqlStorageValue>;

// Explicit read authorization slice. Anonymous internet readers are not a
// synthetic principal and therefore cannot acquire user-scoped state.
export type ReadViewer =
  | { kind: "authenticated"; username: string }
  | { kind: "anonymous" };

export function authenticatedViewer(username: string): ReadViewer {
  return { kind: "authenticated", username };
}

export function anonymousViewer(): ReadViewer {
  return { kind: "anonymous" };
}

// Store wraps the DO's SQL storage. All methods are synchronous; every
// mutation runs inside transactionSync (the analogue of the Go BEGIN…COMMIT
// blocks) — output gates then hold the HTTP response until the write is
// durably committed, so ack ⇒ survives crash holds by construction.
export class Store {
  private transactionDepth = 0;
  private notificationPending = false;

  constructor(
    private storage: DurableObjectStorage,
    // Wakes event transports after a committed event append (the analogue
    // of the Go broadcast channel). Store.notify() defers this callback until
    // the outermost transaction commits, including when a domain mutation is
    // nested inside the idempotency transaction.
    private onNotify: () => void,
  ) {}

  get sql(): SqlStorage {
    return this.storage.sql;
  }

  tx<T>(f: () => T): T {
    const outermost = this.transactionDepth === 0;
    this.transactionDepth++;
    let committed = false;
    try {
      const result = this.storage.transactionSync(f);
      committed = true;
      return result;
    } finally {
      this.transactionDepth--;
      if (outermost) {
        const shouldNotify = committed && this.notificationPending;
        this.notificationPending = false;
        if (shouldNotify) this.onNotify();
      }
    }
  }

  // notify is called by mutations only after they append an event. Outside a
  // transaction it delivers immediately; inside an outer idempotency
  // transaction it is held until transactionSync has returned successfully.
  notify(): void {
    if (this.transactionDepth > 0) {
      this.notificationPending = true;
      return;
    }
    this.onNotify();
  }

  initSchema(): void {
    this.storage.transactionSync(() => {
      this.sql.exec(SCHEMA);
    });
  }
}

export function seqToken(seq: number): string {
  return String(seq);
}

export function isoNow(atMs: number): string {
  return new Date(atMs).toISOString();
}

// newId returns a UUIDv7 (time-sortable for debugging; ordering authority
// remains the event sequence) — parity with the Go server's uuid.NewV7.
export function newId(atMs: number): string {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  // 48-bit big-endian millisecond timestamp.
  b[0] = (atMs / 2 ** 40) & 0xff;
  b[1] = (atMs / 2 ** 32) & 0xff;
  b[2] = (atMs / 2 ** 24) & 0xff;
  b[3] = (atMs / 2 ** 16) & 0xff;
  b[4] = (atMs / 2 ** 8) & 0xff;
  b[5] = atMs & 0xff;
  b[6] = (b[6] & 0x0f) | 0x70; // version 7
  b[8] = (b[8] & 0x3f) | 0x80; // RFC 4122 variant
  const hex = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

// insertEvent appends an event row and returns its sequence. The payload is
// the event's extra fields; seq/type/occurred_at are merged in at read time.
export function insertEvent(
  s: Store,
  type: string,
  threadId: string | null,
  occurredAt: string,
  extra: Record<string, unknown> | null,
): number {
  const payload = JSON.stringify(extra ?? null);
  const row = s.sql
    .exec(
      `INSERT INTO events (type, thread_id, occurred_at, payload) VALUES (?, ?, ?, ?) RETURNING seq`,
      type,
      threadId,
      occurredAt,
      payload,
    )
    .one();
  return row.seq as number;
}

export function updateEventPayload(s: Store, seq: number, extra: Record<string, unknown>): void {
  s.sql.exec(`UPDATE events SET payload = ? WHERE seq = ?`, JSON.stringify(extra), seq);
}

export function userExists(s: Store, username: string): boolean {
  return s.sql.exec(`SELECT 1 FROM users WHERE username = ?`, username).toArray().length > 0;
}

export function insertMentions(s: Store, messageId: string, threadId: string, usernames: string[], seq: number): void {
  for (const u of usernames) {
    s.sql.exec(
      `INSERT INTO mentions (message_id, thread_id, username, seq) VALUES (?, ?, ?, ?)`,
      messageId,
      threadId,
      u,
      seq,
    );
  }
}

// advanceReadCursor moves a user's read cursor forward (never backward) —
// used so one's own posts don't land in one's own inbox. The manual PUT
// (setReadCursor) is absolute instead and may move backward.
export function advanceReadCursor(s: Store, username: string, threadId: string, seq: number): void {
  s.sql.exec(
    `INSERT INTO read_cursors (username, thread_id, seq) VALUES (?, ?, ?)
	 ON CONFLICT (username, thread_id) DO UPDATE SET seq = excluded.seq WHERE excluded.seq > read_cursors.seq`,
    username,
    threadId,
    seq,
  );
}

// currentSeq is the newest sequence in the log (0 when empty).
export function currentSeq(s: Store): number {
  return s.sql.exec(`SELECT COALESCE(MAX(seq), 0) AS cur FROM events`).one().cur as number;
}

// --- threads -----------------------------------------------------------------

const THREAD_COLS = `id, kind, title, tags, creator, created_at, created_seq, last_activity_seq`;

// rowToThread maps the canonical thread column list; participants are loaded
// separately.
export function rowToThread(r: Row): Thread {
  return {
    id: r.id as string,
    kind: r.kind as string,
    title: r.title as string,
    tags: JSON.parse(r.tags as string) as string[],
    creator: r.creator as string,
    created_at: r.created_at as string,
    created_seq: seqToken(r.created_seq as number),
    last_activity_seq: seqToken(r.last_activity_seq as number),
  };
}

export function loadParticipants(s: Store, threadId: string): string[] {
  return s.sql
    .exec(`SELECT username FROM thread_participants WHERE thread_id = ? ORDER BY username`, threadId)
    .toArray()
    .map((r) => r.username as string);
}

// getThread returns a thread if the viewer may see it. DM threads are
// invisible to non-participants: not-found, never forbidden — existence is
// not leaked.
export function getThread(s: Store, id: string, viewer: ReadViewer): Thread {
  const rows = s.sql.exec(`SELECT ${THREAD_COLS} FROM threads WHERE id = ?`, id).toArray();
  if (rows.length === 0) throw new StoreErr("not-found");
  const t = rowToThread(rows[0]);
  if (t.kind === "dm") {
    if (viewer.kind === "anonymous") throw new StoreErr("not-found");
    t.participants = loadParticipants(s, id);
    if (!t.participants.includes(viewer.username)) throw new StoreErr("not-found");
  }
  return t;
}

// createThread creates a thread and its first message in one transaction. A
// non-empty participants set makes it a DM whose membership (participants ∪
// creator, creator first) is permanently fixed. Tags must already be
// normalized.
export function createThread(
  s: Store,
  creator: string,
  title: string,
  content: string,
  tags: string[],
  participants: string[],
  atMs: number,
): { thread: Thread; message: Message } {
  const result = s.tx(() => {
    let kind = "public";
    if (participants.length > 0) {
      kind = "dm";
      for (const p of participants) {
        if (!userExists(s, p)) throw new UnknownParticipantErr(p);
      }
    }

    const ts = isoNow(atMs);
    const threadId = newId(atMs);
    const messageId = newId(atMs);

    // Two placeholder events first: their AUTOINCREMENT seqs are needed
    // inside the payloads (created_seq, last_activity_seq), so the payloads
    // are filled in below, within the same transaction.
    const threadSeq = insertEvent(s, "thread.created", threadId, ts, null);
    const messageSeq = insertEvent(s, "message.created", threadId, ts, null);

    const thread: Thread = {
      id: threadId,
      kind,
      title,
      tags,
      creator,
      ...(participants.length > 0 ? { participants } : {}),
      created_at: ts,
      created_seq: seqToken(threadSeq),
      last_activity_seq: seqToken(messageSeq),
    };
    const mentioned = extractMentions(content, (u) => userExists(s, u));
    const message: Message = {
      id: messageId,
      thread_id: threadId,
      author: creator,
      content,
      ...(mentioned.length > 0 ? { mentions: mentioned } : {}),
      deleted: false,
      created_at: ts,
      seq: seqToken(messageSeq),
      reactions: [],
    };
    updateEventPayload(s, threadSeq, { thread });
    updateEventPayload(s, messageSeq, { message });

    s.sql.exec(
      `INSERT INTO threads (id, kind, title, tags, creator, created_at, created_seq, last_activity_seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
      threadId,
      kind,
      title,
      JSON.stringify(tags),
      creator,
      ts,
      threadSeq,
      messageSeq,
    );
    for (const p of participants) {
      s.sql.exec(`INSERT INTO thread_participants (thread_id, username) VALUES (?, ?)`, threadId, p);
    }
    for (const tag of tags) {
      s.sql.exec(`INSERT INTO thread_tags (thread_id, tag) VALUES (?, ?)`, threadId, tag);
    }
    s.sql.exec(
      `INSERT INTO messages (id, thread_id, author, content, mentions, created_at, created_seq, seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
      messageId,
      threadId,
      creator,
      content,
      JSON.stringify(mentioned),
      ts,
      messageSeq,
      messageSeq,
    );
    insertMentions(s, messageId, threadId, mentioned, messageSeq);
    advanceReadCursor(s, creator, threadId, messageSeq);
    return { thread, message };
  });
  s.notify();
  return result;
}

// listThreads pages through the viewer's visible threads, most recent
// activity first. since=0 means no lower bound; before=0 (the page anchor)
// means start from the top; tags narrows to threads carrying any of them.
export function listThreads(
  s: Store,
  viewer: ReadViewer,
  since: number,
  before: number,
  tags: string[],
  limit: number,
): { items: Thread[]; nextPage: string | null; asOf: string } {
  const asOf = seqToken(currentSeq(s));

  let query = `SELECT ${THREAD_COLS} FROM threads t WHERE `;
  const args: unknown[] = [];
  if (viewer.kind === "authenticated") {
    query += `(t.kind = 'public' OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = t.id AND p.username = ?))`;
    args.push(viewer.username);
  } else {
    query += `t.kind = 'public'`;
  }
  if (since > 0) {
    query += ` AND t.last_activity_seq > ?`;
    args.push(since);
  }
  if (before > 0) {
    query += ` AND t.last_activity_seq < ?`;
    args.push(before);
  }
  if (tags.length > 0) {
    query += ` AND EXISTS (SELECT 1 FROM thread_tags tt WHERE tt.thread_id = t.id AND tt.tag IN (?${", ?".repeat(tags.length - 1)}))`;
    args.push(...tags);
  }
  query += ` ORDER BY t.last_activity_seq DESC LIMIT ?`;
  args.push(limit + 1);

  let items = s.sql.exec(query, ...args).toArray().map(rowToThread);
  let nextPage: string | null = null;
  if (items.length > limit) {
    items = items.slice(0, limit);
    nextPage = items[limit - 1].last_activity_seq;
  }
  for (const t of items) {
    if (t.kind === "dm") t.participants = loadParticipants(s, t.id);
  }
  return { items, nextPage, asOf };
}

// inbox pages through "what needs me": visible threads with activity past
// the viewer's read cursor, where the viewer is a DM participant, a public-
// thread participant (creator or has posted), or has an unread mention —
// plus threads holding unread reactions (by others) to the viewer's own
// messages, which count for the inbox even though they never advance the
// thread's activity cursor. Ordered by most recent inbox-relevant event
// (updated_seq) first; before is the page anchor.
export function inbox(
  s: Store,
  viewer: string,
  before: number,
  limit: number,
): { items: InboxItem[]; nextPage: string | null; asOf: string } {
  const asOf = seqToken(currentSeq(s));

  const query = `SELECT * FROM (
	SELECT t.id, t.kind, t.title, t.tags, t.creator, t.created_at, t.created_seq, t.last_activity_seq,
		rc.seq AS read_seq,
		EXISTS (SELECT 1 FROM mentions mn WHERE mn.thread_id = t.id AND mn.username = ? AND mn.seq > COALESCE(rc.seq, 0)) AS has_mention,
		(t.creator = ? OR EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = t.id AND m.author = ?)) AS is_participant,
		(SELECT MAX(r.seq) FROM reactions r JOIN messages m ON m.id = r.message_id
		 WHERE m.thread_id = t.id AND m.author = ? AND r.username != ? AND r.seq > COALESCE(rc.seq, 0)) AS unread_reaction_seq,
		MAX(CASE WHEN t.last_activity_seq > COALESCE(rc.seq, 0) THEN t.last_activity_seq ELSE 0 END,
		    COALESCE((SELECT MAX(r.seq) FROM reactions r JOIN messages m ON m.id = r.message_id
		              WHERE m.thread_id = t.id AND m.author = ? AND r.username != ? AND r.seq > COALESCE(rc.seq, 0)), 0)) AS updated_seq
	 FROM threads t
	 LEFT JOIN read_cursors rc ON rc.thread_id = t.id AND rc.username = ?
	 WHERE (t.kind = 'public' OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = t.id AND p.username = ?))
	   AND ((t.last_activity_seq > COALESCE(rc.seq, 0)
	         AND (t.kind = 'dm'
	              OR t.creator = ?
	              OR EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = t.id AND m.author = ?)
	              OR EXISTS (SELECT 1 FROM mentions mn WHERE mn.thread_id = t.id AND mn.username = ? AND mn.seq > COALESCE(rc.seq, 0))))
	        OR (SELECT MAX(r.seq) FROM reactions r JOIN messages m ON m.id = r.message_id
	            WHERE m.thread_id = t.id AND m.author = ? AND r.username != ? AND r.seq > COALESCE(rc.seq, 0)) IS NOT NULL)
) WHERE (? = 0 OR updated_seq < ?) ORDER BY updated_seq DESC LIMIT ?`;
  const args: unknown[] = [
    viewer,
    viewer,
    viewer, // has_mention, is_participant
    viewer,
    viewer, // unread_reaction_seq
    viewer,
    viewer, // updated_seq reaction component
    viewer, // read_cursors join
    viewer, // visibility
    viewer,
    viewer,
    viewer, // activity relevance
    viewer,
    viewer, // inclusion reaction component
    before,
    before,
    limit + 1,
  ];

  const rows = s.sql.exec(query, ...args).toArray();
  let items: InboxItem[] = rows.map((r) => {
    const thread = rowToThread(r);
    const readSeq = r.read_seq as number | null;
    const lastActivitySeq = r.last_activity_seq as number;
    const reasons: string[] = [];
    const activityUnread = lastActivitySeq > (readSeq ?? 0);
    if ((r.has_mention as number) !== 0) reasons.push("mention");
    if (activityUnread) {
      if (thread.kind === "dm") reasons.push("dm");
      else if ((r.is_participant as number) !== 0) reasons.push("participant");
    }
    if (r.unread_reaction_seq !== null) reasons.push("reaction");
    return {
      thread,
      reasons,
      updated_seq: seqToken(r.updated_seq as number),
      last_read_seq: readSeq === null ? null : seqToken(readSeq),
    };
  });
  let nextPage: string | null = null;
  if (items.length > limit) {
    items = items.slice(0, limit);
    nextPage = items[limit - 1].updated_seq;
  }
  for (const it of items) {
    if (it.thread.kind === "dm") it.thread.participants = loadParticipants(s, it.thread.id);
  }
  return { items, nextPage, asOf };
}

// getReadCursor returns the viewer's read cursor for a visible thread, null
// when never set.
export function getReadCursor(s: Store, threadId: string, viewer: string): number | null {
  getThread(s, threadId, authenticatedViewer(viewer));
  const rows = s.sql
    .exec(`SELECT seq FROM read_cursors WHERE username = ? AND thread_id = ?`, viewer, threadId)
    .toArray();
  return rows.length === 0 ? null : (rows[0].seq as number);
}

// setReadCursor sets the viewer's read cursor for a visible thread to an
// absolute position — moving backward is allowed (marks things unread).
export function setReadCursor(s: Store, threadId: string, viewer: string, seq: number): void {
  s.tx(() => {
    getThread(s, threadId, authenticatedViewer(viewer));
    s.sql.exec(
      `INSERT INTO read_cursors (username, thread_id, seq) VALUES (?, ?, ?)
	 ON CONFLICT (username, thread_id) DO UPDATE SET seq = excluded.seq`,
      viewer,
      threadId,
      seq,
    );
  });
}

// --- messages (row mapping shared with messages.ts) ---------------------------

export const MESSAGE_COLS = `id, thread_id, author, content, mentions, deleted, created_at, edited_at, deleted_at, deleted_by, created_seq, seq`;

// rowToMessage maps the canonical message column list (MESSAGE_COLS),
// without reaction tallies. Optional fields follow the reference server's
// omitempty marshaling: a tombstone has no content/mentions; deleted_at and
// deleted_by appear only on tombstones.
export function rowToMessage(r: Row): { msg: Message; createdSeq: number } {
  const deleted = (r.deleted as number) !== 0;
  const msg: Message = {
    id: r.id as string,
    thread_id: r.thread_id as string,
    author: r.author as string,
    deleted,
    created_at: r.created_at as string,
    seq: seqToken(r.seq as number),
    reactions: [],
  };
  if (!deleted) {
    const content = r.content as string | null;
    if (content) msg.content = content;
    const mentionsJSON = r.mentions as string | null;
    if (mentionsJSON) {
      const mentions = JSON.parse(mentionsJSON) as string[] | null;
      if (mentions && mentions.length > 0) msg.mentions = mentions;
    }
  }
  if (r.edited_at !== null) msg.edited_at = r.edited_at as string;
  if (r.deleted_at !== null && r.deleted_at !== "") msg.deleted_at = r.deleted_at as string;
  if (r.deleted_by !== null && r.deleted_by !== "") msg.deleted_by = r.deleted_by as string;
  return { msg, createdSeq: r.created_seq as number };
}

// tallies returns a message's per-emoji reaction counts, largest first.
export function tallies(s: Store, messageId: string): ReactionTally[] {
  return s.sql
    .exec(
      `SELECT emoji, COUNT(*) AS count FROM reactions WHERE message_id = ? GROUP BY emoji ORDER BY COUNT(*) DESC, emoji`,
      messageId,
    )
    .toArray()
    .map((r) => ({ emoji: r.emoji as string, count: r.count as number }));
}

// --- events ------------------------------------------------------------------

// EventFilter narrows the events poll — several filters combine as a union;
// the empty value means unfiltered.
export interface EventFilter {
  mentions: boolean; // events whose message mentions the viewer
  dms: boolean; // events in the viewer's DM threads
  subscribedTags: boolean; // events in threads carrying a tag the viewer subscribes to
  tags: string[]; // events in threads carrying any of these tags
}

function filterActive(f: EventFilter): boolean {
  return f.mentions || f.dms || f.subscribedTags || f.tags.length > 0;
}

// events returns up to limit events after the cursor, restricted to the
// viewer's visible slice: everything except DM threads they are not in. The
// returned cursor is the last event's seq, or the request cursor when the
// batch is empty (the dumb-and-safe echo).
export function events(
  s: Store,
  viewer: string,
  after: number,
  limit: number,
  f: EventFilter,
): { events: Event[]; cursor: string } {
  let query = `SELECT e.seq, e.type, e.occurred_at, e.payload
	 FROM events e LEFT JOIN threads t ON t.id = e.thread_id
	 WHERE e.seq > ?
	   AND (e.thread_id IS NULL OR t.kind = 'public'
	        OR EXISTS (SELECT 1 FROM thread_participants p WHERE p.thread_id = e.thread_id AND p.username = ?))`;
  const args: unknown[] = [after, viewer];
  if (filterActive(f)) {
    const clauses: string[] = [];
    if (f.mentions) {
      clauses.push(`e.seq IN (SELECT seq FROM mentions WHERE username = ?)`);
      args.push(viewer);
    }
    if (f.dms) {
      clauses.push(`e.thread_id IN (SELECT thread_id FROM thread_participants WHERE username = ?)`);
      args.push(viewer);
    }
    if (f.subscribedTags) {
      clauses.push(
        `e.thread_id IN (SELECT tt.thread_id FROM thread_tags tt JOIN tag_subscriptions ts ON ts.tag = tt.tag WHERE ts.username = ?)`,
      );
      args.push(viewer);
    }
    if (f.tags.length > 0) {
      clauses.push(`e.thread_id IN (SELECT thread_id FROM thread_tags WHERE tag IN (?${", ?".repeat(f.tags.length - 1)}))`);
      args.push(...f.tags);
    }
    query += ` AND (` + clauses.join(" OR ") + `)`;
  }
  query += ` ORDER BY e.seq LIMIT ?`;
  args.push(limit);

  const rows = s.sql.exec(query, ...args).toArray();
  const out: Event[] = [];
  let cursor = after;
  for (const r of rows) {
    const seq = r.seq as number;
    const payload = r.payload as string;
    const ev: Event = {};
    if (payload !== "" && payload !== "null") {
      Object.assign(ev, JSON.parse(payload) as Record<string, unknown>);
    }
    ev.seq = seqToken(seq);
    ev.type = r.type as string;
    ev.occurred_at = r.occurred_at as string;
    out.push(ev);
    cursor = seq;
  }
  return { events: out, cursor: seqToken(cursor) };
}
