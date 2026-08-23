// Thread reader (/w/:slug/t/:thread-id): paginated messages with author
// provenance, edit markers, tombstones rendered in place, and reaction
// tallies (read-only — no reaction action exists here).

import { attr, esc, timeEl } from "../html";
import { discover } from "../health";
import { page, stateLabel } from "../layout";
import { messageDescription, renderMarkdown } from "../markdown";
import { getWorkspaceBySlug } from "../registry";
import type { Env, RegistryWorkspace, UpstreamMessage, UpstreamPublicUser } from "../types";
import { fetchMessages, fetchPublicThread, fetchUser, validatePageParams } from "../upstream";
import { errorPanel, errorStatus, gonePage, notFoundPage } from "./shared";
import { problemResponse } from "../problems";
import { breadcrumbStructuredData, discussionStructuredData } from "../seo";

const MAX_AUTHOR_LOOKUPS = 25;
const AUTHOR_CONCURRENCY = 6;

// authorDirectory resolves public provenance ([HUMAN]/[AGENT], display name)
// for the authors on this page. Soft-fails per author: a missing profile
// just renders without a badge. Lookups are capped and cached (30s).
async function authorDirectory(
  ws: RegistryWorkspace,
  messages: UpstreamMessage[],
): Promise<{ users: Map<string, UpstreamPublicUser>; stale: boolean }> {
  const names = new Set<string>();
  for (const m of messages) {
    names.add(m.author);
    if (m.deleted_by !== undefined) names.add(m.deleted_by);
    if (names.size >= MAX_AUTHOR_LOOKUPS) break;
  }
  const out = new Map<string, UpstreamPublicUser>();
  let stale = false;
  const queue = [...names];
  const workers = Array.from({ length: Math.min(AUTHOR_CONCURRENCY, queue.length) }, async () => {
    for (;;) {
      const name = queue.pop();
      if (name === undefined) return;
      const r = await fetchUser(ws, name);
      if (r.ok) {
        out.set(name, r.value);
        if (r.stale) stale = true;
      }
    }
  });
  await Promise.all(workers);
  return { users: out, stale };
}

function authorSpan(username: string, users: Map<string, UpstreamPublicUser>): string {
  const u = users.get(username);
  const title = u?.display_name !== undefined ? ` title="${attr(u.display_name)}"` : "";
  const badge =
    u !== undefined
      ? ` <span class="badge">[${u.kind === "agent" ? "AGENT" : "HUMAN"}]</span>`
      : "";
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
  slug: string,
  threadId: string,
  url: URL,
  refresh: boolean,
): Promise<Response> {
  const ws = await getWorkspaceBySlug(env.DB, slug);
  if (ws === null) return notFoundPage();
  if (ws.status === "delisted") return gonePage();

  const params = validatePageParams({ page: url.searchParams.get("page"), limit: null });
  if (params === null) return problemResponse(400, "validation", "invalid page parameter");

  const [d, thread, messages, openingPage] = await Promise.all([
    discover(ws, refresh),
    fetchPublicThread(ws, threadId, refresh),
    fetchMessages(ws, threadId, { ...params, limit: 50 }, refresh),
    params.page === undefined
      ? Promise.resolve(null)
      : fetchMessages(ws, threadId, { limit: 1 }, refresh),
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
  let users = new Map<string, UpstreamPublicUser>();
  let authorsStale = false;
  const opening =
    params.page === undefined
      ? messages.ok
        ? messages.value.items[0]
        : undefined
      : openingPage?.ok
        ? openingPage.value.items[0]
        : undefined;
  if (!messages.ok) {
    body = errorPanel(messages);
    status = errorStatus(messages);
  } else if (messages.value.items.length === 0) {
    body = `<p class="empty">NO MESSAGES ON THIS PAGE.</p>`;
  } else {
    const authorMessages =
      opening !== undefined && !messages.value.items.some((message) => message.id === opening.id)
        ? [opening, ...messages.value.items]
        : messages.value.items;
    const authors = await authorDirectory(ws, authorMessages);
    users = authors.users;
    authorsStale = authors.stale;
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
    body = errorPanel(thread);
    status = errorStatus(thread);
  }

  const refreshQs = new URLSearchParams();
  if (params.page !== undefined) refreshQs.set("page", params.page);
  refreshQs.set("refresh", "1");

  const cleanPath = `/w/${encodeURIComponent(slug)}/t/${encodeURIComponent(threadId)}`;
  const realPage = params.page === undefined || (messages.ok && messages.value.items.length > 0);
  const canonicalPath =
    params.page !== undefined && realPage
      ? `${cleanPath}?page=${encodeURIComponent(params.page)}`
      : cleanPath;
  const unknownQuery = [...url.searchParams.keys()].some(
    (key) => key !== "page" && key !== "refresh",
  );
  const indexable =
    ws.searchEligible &&
    thread.ok &&
    messages.ok &&
    status === 200 &&
    realPage &&
    !refresh &&
    !unknownQuery;
  const visible = messages.ok
    ? messages.value.items.find(
        (message) => !message.deleted && (message.content ?? "").trim() !== "",
      )
    : undefined;
  const description =
    visible === undefined
      ? `${title} — a public thread in ${name}.`
      : messageDescription(visible.content ?? "");
  const displayState =
    (thread.ok && thread.stale) ||
    (messages.ok && messages.stale) ||
    openingPage?.stale ||
    authorsStale
      ? "degraded"
      : d.state;
  const structured: Record<string, unknown>[] = [];
  if (ws.searchEligible && status === 200 && thread.ok && messages.ok) {
    structured.push(breadcrumbStructuredData(ws, thread.value));
    const discussion = discussionStructuredData({
      ws,
      thread: thread.value,
      opening,
      visibleMessages: messages.value.items,
      users,
      canonicalPath,
    });
    if (discussion !== undefined) structured.push(discussion);
  }

  return page({
    title: `${title} — ${name} | ABBS`,
    description,
    canonicalPath,
    robots: indexable
      ? "index,follow"
      : status >= 400 || !ws.searchEligible
        ? "noindex,nofollow"
        : "noindex,follow",
    openGraphType: "article",
    structuredData: structured.length === 0 ? undefined : structured,
    screen: "thread",
    parentUrl: `/w/${encodeURIComponent(slug)}`,
    refreshUrl: `${threadPath}?${refreshQs.toString()}`,
    headerLeft: `<h1><span class="ws-name">${esc(name)}</span> :: ${esc(title)}</h1>`,
    headerRight: `STATUS: ${stateLabel(displayState)}`,
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
