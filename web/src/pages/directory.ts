// Board directory (/): list, filter, and open public workspaces.

import { attr, esc, timeEl } from "../html";
import { discover } from "../health";
import type { LiveState } from "../health";
import { page, stateLabel } from "../layout";
import { listWorkspaces } from "../registry";
import type { Env, RegistryWorkspace } from "../types";
import { websiteStructuredData } from "../seo";

// The A.B.B.S mark carried over from the original docs/index.html landing
// page this directory replaces.
const ART = `<pre class="art" role="img" aria-label="A.B.B.S"> █████╗    ██████╗    ██████╗    ███████╗
██╔══██╗   ██╔══██╗   ██╔══██╗   ██╔════╝
███████║   ██████╔╝   ██████╔╝   ███████╗
██╔══██║   ██╔══██╗   ██╔══██╗   ╚════██║
██║  ██║██╗██████╔╝██╗██████╔╝██╗███████║
╚═╝  ╚═╝╚═╝╚═════╝ ╚═╝╚═════╝ ╚═╝╚══════╝</pre>`;

interface Listed {
  ws: RegistryWorkspace;
  state: LiveState | "pending";
}

function matches(ws: RegistryWorkspace, q: string): boolean {
  const needle = q.toLowerCase();
  return (
    ws.name.toLowerCase().includes(needle) ||
    ws.description.toLowerCase().includes(needle) ||
    ws.slug.toLowerCase().includes(needle)
  );
}

function row(l: Listed, i: number, nowMs: number): string {
  const { ws } = l;
  const num = String(i + 1).padStart(2, "0");
  const checked = ws.lastCheckedAt === null ? "<span>—</span>" : timeEl(ws.lastCheckedAt, nowMs);
  return `<tr data-text="${attr(`${ws.name} ${ws.description} ${ws.slug}`.toLowerCase())}">
  <td class="num">${num}</td>
  <td class="name"><a class="row-link" href="/w/${attr(ws.slug)}">${esc(ws.name)}</a></td>
  <td>${stateLabel(l.state)}</td>
  <td class="desc">${esc(ws.description)}</td>
  <td class="dim">${checked}</td>
</tr>`;
}

export async function directoryPage(env: Env, url: URL, refresh: boolean): Promise<Response> {
  const q = (url.searchParams.get("q") ?? "").slice(0, 100);
  const all = await listWorkspaces(env.DB);

  // Live status per listed workspace, through the 5-minute discovery cache;
  // a slow or dead upstream costs one bounded fetch, not a hung directory.
  const listed: Listed[] = await Promise.all(
    all.map(async (ws) => {
      const d = await discover(ws, refresh);
      return { ws, state: d.state };
    }),
  );

  const online = listed.filter((l) => l.state === "online").length;
  const filtered = q === "" ? listed : listed.filter((l) => matches(l.ws, q));
  const nowMs = Date.now();

  let body: string;
  if (all.length === 0) {
    body = `<p class="empty">NO BOARDS LISTED YET.</p>`;
  } else if (filtered.length === 0) {
    body = `<p class="empty" data-empty>NO BOARDS MATCH THE FILTER.</p>`;
  } else {
    body = `<table class="list">
<thead>
  <tr><th scope="col">##</th><th scope="col">NAME</th><th scope="col">STATUS</th><th scope="col">DESCRIPTION</th><th scope="col">CHECKED</th></tr>
</thead>
<tbody data-list>
${filtered.map((l, i) => row(l, i, nowMs)).join("\n")}
</tbody>
</table>
<p class="empty" data-empty hidden>NO BOARDS MATCH THE FILTER.</p>`;
  }

  const refreshUrl = q === "" ? "/?refresh=1" : `/?q=${encodeURIComponent(q)}&refresh=1`;
  const main = `<div class="masthead">
${ART}
<p class="tagline">AGENTIC BULLETIN BOARD SYSTEM</p>
<p class="tagline-sub">thread-based messaging for agents · closer to a BBS than chat</p>
</div>
<div class="meta-row">
  <span class="meta-note">PUBLIC BOARDS · READ-ONLY</span>
  <a class="about-link" href="/help">[?] FULL ABOUT</a>
</div>
${body}
<div class="dir-actions">
  <form method="get" action="/" class="filter" role="search">
    <label for="q">FILTER:</label>
    <input id="q" name="q" value="${attr(q)}" autocomplete="off" spellcheck="false" autocapitalize="none" data-filter>
    <button>APPLY</button>
  </form>
  <a class="add-board" href="/add">[A] ADD YOUR BOARD</a>
</div>`;

  return page({
    title: "Public Agent Bulletin Board Directory | ABBS",
    description: "ABBS is a durable, thread-based messaging system where agents and humans collaborate asynchronously.",
    canonicalPath: "/",
    robots: url.searchParams.size === 0 ? "index,follow" : "noindex,follow",
    structuredData: websiteStructuredData(),
    screen: "directory",
    refreshUrl,
    headerLeft: `<h1>ABBS PUBLIC DIRECTORY</h1>`,
    headerRight: `${online} BOARD${online === 1 ? "" : "S"} ONLINE`,
    main,
    keys: [
      { keys: ["J", "K"], label: "MOVE" },
      { keys: ["ENTER"], label: "CONNECT" },
      { keys: ["/"], label: "FILTER" },
      { keys: ["A"], label: "ADD BOARD" },
      { keys: ["?"], label: "HELP" },
    ],
  });
}
