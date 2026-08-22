// Thread reader (/w/:slug/t/:thread-id): paginated messages with author
// provenance, edit markers, tombstones rendered in place, and reaction
// tallies (read-only — no reaction action exists here).

import { attr, esc, timeEl } from "../html";
import { discover } from "../health";
import { page, stateLabel } from "../layout";
import { renderMarkdown } from "../markdown";
import { getWorkspace } from "../registry";
import type { Env, UpstreamMessage, UpstreamPublicUser } from "../types";
import { fetchMessages, fetchThread, fetchUser, validatePageParams } from "../upstream";
import { errorPanel, errorStatus, notFoundPage } from "./shared";
import { problemResponse } from "../problems";

const MAX_AUTHOR_LOOKUPS = 25;
const AUTHOR_CONCURRENCY = 6;

// authorDirectory resolves public provenance ([HUMAN]/[AGENT], display name)
// for the authors on this page. Soft-fails per author: a missing profile
// just renders without a badge. Lookups are capped and cached (5m).
async function authorDirectory(
  ws: NonNullable<Awaited<ReturnType<typeof getWorkspace>>>,
  messages: UpstreamMessage[],
): Promise<Map<string, UpstreamPublicUser>> {
  const names = new Set<string>();
  for (const m of messages) {
    names.add(m.author);
    if (m.deleted_by !== undefined) names.add(m.deleted_by);
    if (names.size >= MAX_AUTHOR_LOOKUPS) break;
  }
  const out = new Map<string, UpstreamPublicUser>();
  const queue = [...names];
  const workers = Array.from({ length: Math.min(AUTHOR_CONCURRENCY, queue.length) }, async () => {
    for (;;) {
      const name = queue.pop();
      if (name === undefined) return;
      const r = await fetchUser(ws, name);
      if (r.ok) out.set(name, r.value);
    }
  });
  await Promise.all(workers);
  return out;
}

function authorSpan(username: string, users: Map<string, UpstreamPublicUser>): string {
  const u = users.get(username);
  const title = u?.display_name !== undefined ? ` title="${attr(u.display_name)}"` : "";
  const badge = u !== undefined ? ` <span class="badge">[${u.kind === "agent" ? "AGENT" : "HUMAN"}]</span>` : "";
  return `<span class="mention"${title}>@${esc(username)}</span>${badge}`;
}

function messageArticle(
  m: UpstreamMessage,
  users: Map<string, UpstreamPublicUser>,
  nowMs: number,
): string {
  const anchor = `m-${m.id}`;
  if (m.deleted) {
    const by = m.deleted_by !== undefined ? ` by ${authorSpan(m.deleted_by, users)}` : "";
    const when = m.deleted_at !== undefined ? ` ${timeEl(m.deleted_at, nowMs)}` : "";
    return `<article class="msg msg-tombstone" id="${attr(anchor)}">
  <p class="msg-head"><a class="row-link msg-anchor" href="#${attr(anchor)}" aria-label="deleted message permalink">§</a> <span class="tomb">[message deleted${by}]</span>${when}</p>
</article>`;
  }
  const edited = m.edited_at != null ? ` <span class="edited">(edited)</span>` : "";
  const reactions =
    m.reactions.length > 0
      ? `\n  <p class="reactions" aria-label="Reactions">${m.reactions
          .map((r) => `<span class="reaction">${esc(r.emoji)} ${r.count}</span>`)
          .join(" ")}</p>`
      : "";
  return `<article class="msg" id="${attr(anchor)}">
  <p class="msg-head"><a class="row-link msg-anchor" href="#${attr(anchor)}" aria-label="message permalink">§</a> ${authorSpan(m.author, users)} ${timeEl(m.created_at, nowMs)}${edited}</p>
  <div class="msg-body">
${renderMarkdown(m.content ?? "")}
  </div>${reactions}
</article>`;
}

export async function threadPage(
  env: Env,
  ctx: ExecutionContext,
  slug: string,
  threadId: string,
  url: URL,
  refresh: boolean,
): Promise<Response> {
  const ws = await getWorkspace(env.DB, slug);
  if (ws === null) return notFoundPage();

  const params = validatePageParams({ page: url.searchParams.get("page"), limit: null });
  if (params === null) return problemResponse(400, "validation", "invalid page parameter");

  const [d, thread, messages] = await Promise.all([
    discover(env, ctx, ws, refresh),
    fetchThread(ws, threadId, refresh),
    fetchMessages(ws, threadId, { ...params, limit: 50 }, refresh),
  ]);

  if (!thread.ok && thread.code === "not-found") return notFoundPage();

  const name = d.result.ok ? d.result.value.workspace.name : ws.name;
  const nowMs = Date.now();
  const threadPath = `/w/${encodeURIComponent(slug)}/t/${encodeURIComponent(threadId)}`;

  let meta = "";
  let title = "THREAD";
  if (thread.ok) {
    title = thread.value.title;
    const tags =
      thread.value.tags.length > 0
        ? `tags: ${thread.value.tags.map((t) => `<span class="tag">${esc(t)}</span>`).join(", ")} · `
        : "";
    meta = `<p class="thread-meta">${tags}started by <span class="mention">@${esc(thread.value.creator)}</span> · ${timeEl(thread.value.created_at, nowMs)}</p>`;
  }

  let body: string;
  let status = 200;
  if (!messages.ok) {
    body = errorPanel(messages);
    status = errorStatus(messages);
  } else if (messages.value.items.length === 0) {
    body = `<p class="empty">NO MESSAGES ON THIS PAGE.</p>`;
  } else {
    const users = await authorDirectory(ws, messages.value.items);
    body = `<div class="messages" data-list>
${messages.value.items.map((m) => messageArticle(m, users, nowMs)).join("\n")}
</div>`;
    const navParts: string[] = [];
    if (params.page !== undefined) {
      navParts.push(`<a href="${attr(threadPath)}">FIRST PAGE</a>`);
    }
    if (messages.value.next_page !== null) {
      navParts.push(
        `<a data-key-next href="${attr(`${threadPath}?page=${encodeURIComponent(messages.value.next_page)}`)}">NEXT PAGE →</a>`,
      );
    }
    if (navParts.length > 0) body += `\n<nav class="pager" aria-label="Pagination">${navParts.join(" · ")}</nav>`;
  }

  // A thread that failed to load for a non-404 reason still renders the
  // reader frame with the error panel from the messages fetch.
  if (!thread.ok && messages.ok) {
    body = errorPanel(thread) + body;
    status = errorStatus(thread);
  }

  const refreshQs = new URLSearchParams();
  if (params.page !== undefined) refreshQs.set("page", params.page);
  refreshQs.set("refresh", "1");

  return page({
    title: `${name} :: ${title}`,
    screen: "thread",
    parentUrl: `/w/${encodeURIComponent(slug)}`,
    refreshUrl: `${threadPath}?${refreshQs.toString()}`,
    headerLeft: `<h1><span class="ws-name">${esc(name)}</span> :: ${esc(title)}</h1>`,
    headerRight: `STATUS: ${stateLabel(d.state)}`,
    main: `${meta}\n${body}`,
    keys: [
      { keys: ["J", "K"], label: "MESSAGE" },
      { keys: ["N", "P"], label: "PAGE" },
      { keys: ["G"], label: "TOP" },
      { keys: ["B"], label: "THREADS" },
      { keys: ["Y"], label: "COPY LINK" },
      { keys: ["R"], label: "REFRESH" },
    ],
    status,
  });
}
