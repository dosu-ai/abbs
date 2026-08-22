// Workspace board (/w/:slug): workspace metadata, status, tags, threads.
// Thread order is exactly the workspace's own: most recent activity first.

import { attr, esc, timeEl } from "../html";
import { discover } from "../health";
import { page, stateLabel } from "../layout";
import { getWorkspace } from "../registry";
import type { Env, RegistryWorkspace, UpstreamThread } from "../types";
import { fetchTags, fetchThreads, validatePageParams } from "../upstream";
import { errorPanel, errorStatus, notFoundPage } from "./shared";
import { problemResponse } from "../problems";

const TAG_ROW_MAX = 16;

function boardUrl(slug: string, p: { tag?: string; page?: string; q?: string; refresh?: boolean }): string {
  const qs = new URLSearchParams();
  if (p.tag !== undefined && p.tag !== "") qs.set("tag", p.tag);
  if (p.page !== undefined && p.page !== "") qs.set("page", p.page);
  if (p.q !== undefined && p.q !== "") qs.set("q", p.q);
  if (p.refresh === true) qs.set("refresh", "1");
  const s = qs.toString();
  return `/w/${encodeURIComponent(slug)}${s === "" ? "" : "?" + s}`;
}

function threadRow(ws: RegistryWorkspace, t: UpstreamThread, nowMs: number): string {
  const tags = t.tags.map((tag) => `<span class="tag">${esc(tag)}</span>`).join(" ");
  const text = `${t.title} ${t.tags.join(" ")} ${t.creator}`.toLowerCase();
  return `<tr data-text="${attr(text)}">
  <td class="title"><a class="row-link" href="/w/${attr(ws.slug)}/t/${attr(t.id)}">${esc(t.title)}</a></td>
  <td class="tags">${tags}</td>
  <td class="by"><span class="mention">@${esc(t.creator)}</span></td>
  <td class="dim">${timeEl(t.created_at, nowMs)}</td>
</tr>`;
}

export async function boardPage(
  env: Env,
  ctx: ExecutionContext,
  slug: string,
  url: URL,
  refresh: boolean,
): Promise<Response> {
  const ws = await getWorkspace(env.DB, slug);
  if (ws === null) return notFoundPage();

  const q = (url.searchParams.get("q") ?? "").slice(0, 100);
  const tag = (url.searchParams.get("tag") ?? "").slice(0, 64);
  const params = validatePageParams({
    page: url.searchParams.get("page"),
    limit: null,
    tags: tag === "" ? undefined : [tag],
  });
  if (params === null) {
    return problemResponse(400, "validation", "invalid page or tag parameter");
  }

  const [d, threads, tags] = await Promise.all([
    discover(env, ctx, ws, refresh),
    fetchThreads(ws, { ...params, limit: 50 }, refresh),
    fetchTags(ws, { limit: 50 }, refresh),
  ]);

  const name = d.result.ok ? d.result.value.workspace.name : ws.name;
  const description = d.result.ok ? (d.result.value.workspace.description ?? "") : ws.description;
  const nowMs = Date.now();

  // Tag row: soft-fails to nothing — the board still works without it.
  let tagRow = "";
  if (tags.ok && tags.value.items.length > 0) {
    const links = tags.value.items
      .slice(0, TAG_ROW_MAX)
      .map((t) => {
        const current = t.name === tag;
        return `<a class="tag-link"${current ? ` aria-current="true"` : ""} href="${attr(boardUrl(slug, { tag: t.name }))}">[${esc(t.name)} ${t.thread_count}]</a>`;
      })
      .join(" ");
    const all = `<a class="tag-link"${tag === "" ? ` aria-current="true"` : ""} href="${attr(boardUrl(slug, {}))}">[all]</a>`;
    tagRow = `<nav class="tag-row" aria-label="Filter threads by tag">TAGS: ${all} ${links}</nav>`;
  }

  let body: string;
  let status = 200;
  if (!threads.ok) {
    body = errorPanel(threads);
    status = errorStatus(threads);
  } else {
    const items =
      q === ""
        ? threads.value.items
        : threads.value.items.filter((t) =>
            `${t.title} ${t.tags.join(" ")} ${t.creator}`.toLowerCase().includes(q.toLowerCase()),
          );
    if (threads.value.items.length === 0) {
      body = `<p class="empty">${tag === "" ? "NO PUBLIC THREADS YET." : "NO THREADS CARRY THIS TAG."}</p>`;
    } else if (items.length === 0) {
      body = `<p class="empty" data-empty>NO THREADS MATCH THE FILTER ON THIS PAGE.</p>`;
    } else {
      body = `<table class="list">
<thead>
  <tr><th scope="col">TITLE</th><th scope="col">TAGS</th><th scope="col">BY</th><th scope="col">STARTED</th></tr>
</thead>
<tbody data-list>
${items.map((t) => threadRow(ws, t, nowMs)).join("\n")}
</tbody>
</table>
<p class="empty" data-empty hidden>NO THREADS MATCH THE FILTER ON THIS PAGE.</p>`;
    }
    const navParts: string[] = [];
    if (params.page !== undefined) {
      navParts.push(`<a href="${attr(boardUrl(slug, { tag, q }))}">FIRST PAGE</a>`);
    }
    if (threads.value.next_page !== null) {
      navParts.push(
        `<a data-key-next href="${attr(boardUrl(slug, { tag, q, page: threads.value.next_page }))}">NEXT PAGE →</a>`,
      );
    }
    if (navParts.length > 0) body += `\n<nav class="pager" aria-label="Pagination">${navParts.join(" · ")}</nav>`;
  }

  const filterForm = `<form method="get" action="${attr(boardUrl(slug, {}))}" class="filter" role="search">
  ${tag !== "" ? `<input type="hidden" name="tag" value="${attr(tag)}">` : ""}
  <label for="q">FILTER:</label>
  <input id="q" name="q" value="${attr(q)}" autocomplete="off" spellcheck="false" autocapitalize="none" data-filter>
  <button>APPLY</button>
</form>`;

  const main = `<p class="desc">${esc(description)}</p>
${tagRow}
${filterForm}
${body}`;

  return page({
    title: `${name} / PUBLIC THREADS`,
    description: description === "" ? undefined : description,
    screen: "board",
    parentUrl: "/",
    refreshUrl: boardUrl(slug, { tag, q, page: params.page, refresh: true }),
    headerLeft: `<h1>CONNECTED: <span class="ws-name">${esc(name)}</span> / PUBLIC THREADS</h1>`,
    headerRight: `STATUS: ${stateLabel(d.state)}`,
    main,
    keys: [
      { keys: ["J", "K"], label: "MOVE" },
      { keys: ["ENTER"], label: "READ" },
      { keys: ["/"], label: "FILTER" },
      { keys: ["N"], label: "NEXT PAGE" },
      { keys: ["B"], label: "BOARDS" },
      { keys: ["R"], label: "REFRESH" },
    ],
    status,
  });
}
