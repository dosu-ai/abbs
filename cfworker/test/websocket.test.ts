import { SELF, env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { WorkspaceDO } from "../src/workspace-do";
import { deactivateUser } from "../src/store/users";

const BASE = "http://abbs.test";

interface EventFrame {
  seq: string;
  type: string;
  [key: string]: unknown;
}

interface SocketAttachment {
  user: string;
  tokenHash: string;
  cursor: number;
  filter: {
    mentions: boolean;
    dms: boolean;
    subscribedTags: boolean;
    tags: string[];
  };
}

interface SocketReader {
  next(): Promise<EventFrame>;
  read(count: number): Promise<EventFrame[]>;
}

async function claim(username: string): Promise<string> {
  const response = await SELF.fetch(`${BASE}/v1/users`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, kind: "agent" }),
  });
  expect(response.status).toBe(201);
  return ((await response.json()) as { token: string }).token;
}

function auth(token: string): Record<string, string> {
  return { Authorization: `Bearer ${token}` };
}

async function drainCursor(token: string): Promise<string> {
  let cursor = "0";
  for (;;) {
    const q = new URLSearchParams({ cursor, limit: "100" });
    const response = await SELF.fetch(`${BASE}/v1/events?${q}`, { headers: auth(token) });
    expect(response.status).toBe(200);
    const batch = (await response.json()) as { events: EventFrame[]; cursor: string };
    cursor = batch.cursor;
    if (batch.events.length === 0) return cursor;
  }
}

async function pollAfter(token: string, start: string): Promise<EventFrame[]> {
  const out: EventFrame[] = [];
  let cursor = start;
  for (;;) {
    const q = new URLSearchParams({ cursor, limit: "100" });
    const response = await SELF.fetch(`${BASE}/v1/events?${q}`, { headers: auth(token) });
    expect(response.status).toBe(200);
    const batch = (await response.json()) as { events: EventFrame[]; cursor: string };
    out.push(...batch.events);
    if (batch.events.length === 0) return out;
    cursor = batch.cursor;
  }
}

function makeReader(socket: WebSocket): SocketReader {
  const queued: EventFrame[] = [];
  const waiting: Array<{
    resolve: (event: EventFrame) => void;
    reject: (error: Error) => void;
    timer: ReturnType<typeof setTimeout>;
  }> = [];
  let closed: CloseEvent | null = null;

  socket.addEventListener("message", (message) => {
    if (typeof message.data !== "string") return;
    const event = JSON.parse(message.data) as EventFrame;
    const waiter = waiting.shift();
    if (waiter === undefined) {
      queued.push(event);
      return;
    }
    clearTimeout(waiter.timer);
    waiter.resolve(event);
  });
  socket.addEventListener("close", (event) => {
    closed = event;
    for (const waiter of waiting.splice(0)) {
      clearTimeout(waiter.timer);
      waiter.reject(new Error(`websocket closed ${event.code}: ${event.reason}`));
    }
  });

  const next = (): Promise<EventFrame> => {
    const event = queued.shift();
    if (event !== undefined) return Promise.resolve(event);
    if (closed !== null) return Promise.reject(new Error(`websocket closed ${closed.code}: ${closed.reason}`));
    return new Promise<EventFrame>((resolve, reject) => {
      const timer = setTimeout(() => {
        const index = waiting.findIndex((entry) => entry.resolve === resolve);
        if (index >= 0) waiting.splice(index, 1);
        reject(new Error("timed out waiting for websocket event"));
      }, 5000);
      waiting.push({ resolve, reject, timer });
    });
  };

  return {
    next,
    async read(count: number): Promise<EventFrame[]> {
      const events: EventFrame[] = [];
      for (let i = 0; i < count; i++) events.push(await next());
      return events;
    },
  };
}

async function connect(token: string, query?: URLSearchParams): Promise<{ socket: WebSocket; reader: SocketReader }> {
  const suffix = query === undefined || query.size === 0 ? "" : `?${query}`;
  const response = await SELF.fetch(`${BASE}/v1/events/ws${suffix}`, {
    headers: { ...auth(token), Upgrade: "websocket" },
  });
  expect(response.status).toBe(101);
  const socket = response.webSocket;
  if (socket === null) throw new Error("websocket upgrade returned no client socket");
  const reader = makeReader(socket);
  socket.accept();
  return { socket, reader };
}

async function createThread(
  token: string,
  title: string,
  tags: string[] = [],
): Promise<{ id: string; last_activity_seq: string }> {
  const response = await SELF.fetch(`${BASE}/v1/threads`, {
    method: "POST",
    headers: { ...auth(token), "Content-Type": "application/json" },
    body: JSON.stringify({ title, content: "first", ...(tags.length > 0 ? { tags } : {}) }),
  });
  expect(response.status).toBe(201);
  return (await response.json()) as { id: string; last_activity_seq: string };
}

function waitForClose(socket: WebSocket): Promise<CloseEvent> {
  return new Promise<CloseEvent>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("timed out waiting for websocket close")), 5000);
    socket.addEventListener("close", (event) => {
      clearTimeout(timer);
      resolve(event);
    });
  });
}

function workspaceStub(): DurableObjectStub {
  return env.WORKSPACE.get(env.WORKSPACE.idFromName(env.WORKSPACE_NAME ?? "abbs"));
}

async function attachmentsFor(user: string): Promise<SocketAttachment[]> {
  return runInDurableObject(workspaceStub(), (_instance, state) =>
    state
      .getWebSockets()
      .map((socket) => socket.deserializeAttachment() as SocketAttachment | null)
      .filter((attachment): attachment is SocketAttachment => attachment?.user === user),
  );
}

describe("events websocket", () => {
  it("advertises the capability and sends catch-up plus live events verbatim", async () => {
    const writer = await claim("ws-live-writer");
    const readerToken = await claim("ws-live-reader");
    const start = await drainCursor(readerToken);
    const thread = await createThread(writer, "ws live");
    const wantCatchUp = await pollAfter(readerToken, start);
    expect(wantCatchUp).toHaveLength(2);

    const infoResponse = await SELF.fetch(`${BASE}/v1/server`);
    const info = (await infoResponse.json()) as { capabilities?: string[] };
    expect(info.capabilities).toContain("websocket");

    const { socket, reader } = await connect(readerToken, new URLSearchParams({ cursor: start }));
    expect(await reader.read(wantCatchUp.length)).toEqual(wantCatchUp);

    // Client application frames are reserved for future evolution and are
    // ignored; they must not turn into a policy close.
    socket.send(JSON.stringify({ future_client_message: true }));
    const live = reader.next();
    const posted = await SELF.fetch(`${BASE}/v1/threads/${thread.id}/messages`, {
      method: "POST",
      headers: { ...auth(writer), "Content-Type": "application/json" },
      body: JSON.stringify({ content: "live tail" }),
    });
    expect(posted.status).toBe(201);
    expect((await live).type).toBe("message.created");
    socket.close(1000, "done");
  });

  it("round-trips attachments and advances each socket cursor independently", async () => {
    const writer = await claim("ws-cursor-writer");
    const readerToken = await claim("ws-cursor-reader");
    const start = await drainCursor(readerToken);
    const all = await connect(readerToken, new URLSearchParams({ cursor: start }));
    const tagged = await connect(readerToken, new URLSearchParams({ cursor: start, tag: "important" }));

    let attachments = await attachmentsFor("ws-cursor-reader");
    expect(attachments).toHaveLength(2);
    expect(attachments.map((a) => a.cursor)).toEqual([Number(start), Number(start)]);
    expect(attachments.every((a) => a.tokenHash.length === 64)).toBe(true);
    expect(attachments.map((a) => a.filter.tags).sort((a, b) => a.length - b.length)).toEqual([[], ["important"]]);

    const untaggedThread = await createThread(writer, "untagged");
    expect(await all.reader.read(2)).toHaveLength(2);
    attachments = await attachmentsFor("ws-cursor-reader");
    const allAttachment = attachments.find((a) => a.filter.tags.length === 0);
    const tagAttachment = attachments.find((a) => a.filter.tags.length === 1);
    expect(allAttachment?.cursor).toBe(Number(untaggedThread.last_activity_seq));
    expect(tagAttachment?.cursor).toBe(Number(start));

    const taggedThread = await createThread(writer, "tagged", ["important"]);
    expect(await all.reader.read(2)).toHaveLength(2);
    const taggedEvents = await tagged.reader.read(2);
    expect(taggedEvents.map((event) => event.type)).toEqual(["thread.created", "message.created"]);
    attachments = await attachmentsFor("ws-cursor-reader");
    expect(attachments.every((a) => a.cursor === Number(taggedThread.last_activity_seq))).toBe(true);

    all.socket.close(1000, "done");
    tagged.socket.close(1000, "done");
  });

  it("closes a deactivated principal with policy code 1008", async () => {
    const token = await claim("ws-deactivated");
    const start = await drainCursor(token);
    const { socket } = await connect(token, new URLSearchParams({ cursor: start }));
    const closed = waitForClose(socket);

    await runInDurableObject(workspaceStub(), (instance) => {
      deactivateUser((instance as unknown as WorkspaceDO).store, "ws-deactivated", Date.now());
    });

    const event = await closed;
    expect(event.code).toBe(1008);
    expect(event.reason).toContain("deactivated");
  });

  it("returns ordinary handshake problems and shares poll filter validation", async () => {
    const token = await claim("ws-problems");

    const unauthorized = await SELF.fetch(`${BASE}/v1/events/ws`, { headers: { Upgrade: "websocket" } });
    expect(unauthorized.status).toBe(401);
    expect(unauthorized.headers.get("Content-Type")).toContain("application/problem+json");

    const badCursor = await SELF.fetch(`${BASE}/v1/events/ws?cursor=banana`, {
      headers: { ...auth(token), Upgrade: "websocket" },
    });
    expect(badCursor.status).toBe(400);

    const badWebSocketFilter = await SELF.fetch(`${BASE}/v1/events/ws?mentions=maybe`, {
      headers: { ...auth(token), Upgrade: "websocket" },
    });
    expect(badWebSocketFilter.status).toBe(400);
    const badPollFilter = await SELF.fetch(`${BASE}/v1/events?mentions=maybe`, { headers: auth(token) });
    expect(badPollFilter.status).toBe(400);

    const plain = await SELF.fetch(`${BASE}/v1/events/ws`, { headers: auth(token) });
    expect(plain.status).toBe(400);
    const problem = (await plain.json()) as { detail: string };
    expect(problem.detail).toContain("Upgrade: websocket");
  });

  it("refuses an attachment over 2 KiB before accepting the socket", async () => {
    const token = await claim("ws-large-filter");
    const query = new URLSearchParams();
    for (let i = 0; i < 16; i++) {
      query.append("tag", String.fromCodePoint(0x1f600 + i) + "😀".repeat(63));
    }
    const response = await SELF.fetch(`${BASE}/v1/events/ws?${query}`, {
      headers: { ...auth(token), Upgrade: "websocket" },
    });
    expect(response.status).toBe(400);
    expect(response.webSocket).toBeNull();
    const problem = (await response.json()) as { type: string; detail: string };
    expect(problem.type).toBe("https://abbs.dev/problems/validation");
    expect(problem.detail).toBe(
      "tag filter too large for the websocket transport on this server; narrow the tag filter or use GET /v1/events",
    );
    expect(await attachmentsFor("ws-large-filter")).toHaveLength(0);
  });
});
