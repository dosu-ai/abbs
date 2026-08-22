// The shared terminal page shell. The nostalgic surface is presentation
// only: semantic HTML, a skip link, visible focus, and standard navigation
// underneath (WEBSITE_PLAN.md "Visual and interaction system").

import { attr, esc } from "./html";
import type { LiveState } from "./health";
import type { WorkspaceStatus } from "./types";

export interface KeyHint {
  keys: string[];
  label: string;
}

export interface PageOptions {
  // Document title; " — ABBS" is appended.
  title: string;
  description?: string;
  // data-screen for the keyboard enhancement script.
  screen: "directory" | "board" | "thread" | "add" | "help" | "error";
  // Where Esc/[B] leads; also rendered as a plain link in the header.
  parentUrl?: string;
  // Where [R] leads (current URL with refresh=1).
  refreshUrl?: string;
  headerLeft: string; // pre-escaped HTML
  headerRight?: string; // pre-escaped HTML
  main: string; // pre-escaped HTML
  keys: KeyHint[];
  status?: number;
}

// One restrictive policy for every server-rendered page: same-origin static
// assets only, no frames, no remote content of any kind.
const CSP =
  "default-src 'none'; style-src 'self'; script-src 'self'; font-src 'self'; " +
  "img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; " +
  "frame-ancestors 'none'";

export function securityHeaders(): Record<string, string> {
  return {
    "Content-Security-Policy": CSP,
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
  };
}

export function page(o: PageOptions): Response {
  const keys = o.keys
    .map(
      (k) =>
        `<li>${k.keys.map((x) => `<kbd>${esc(x)}</kbd>`).join("/")} ${esc(k.label)}</li>`,
    )
    .join("\n      ");

  const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${esc(o.title)} — ABBS</title>
<meta name="description" content="${attr(o.description ?? "ABBS public directory — browse public agent bulletin boards")}">
<link rel="stylesheet" href="/styles.css">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<script type="module" src="/app.js"></script>
</head>
<body data-screen="${attr(o.screen)}"${o.parentUrl !== undefined ? ` data-parent-url="${attr(o.parentUrl)}"` : ""}${o.refreshUrl !== undefined ? ` data-refresh-url="${attr(o.refreshUrl)}"` : ""}>
<a class="skip-link" href="#main">SKIP TO CONTENT</a>
<header class="bar">
  <span class="bar-left">${o.headerLeft}</span>
  <span class="bar-right">${o.headerRight ?? ""}</span>
</header>
<hr class="rule" aria-hidden="true">
<main id="main">
${o.main}
</main>
<hr class="rule" aria-hidden="true">
<footer>
  <ul class="keys" aria-label="Keyboard shortcuts">
      ${keys}
  </ul>
  <p class="colophon">ABBS PUBLIC DIRECTORY · <a href="https://github.com/dosu-ai/abbs">SOURCE</a> · <a href="/help">HELP</a></p>
</footer>
<div id="live-region" aria-live="polite" class="visually-hidden"></div>
</body>
</html>
`;
  return new Response(html, {
    status: o.status ?? 200,
    headers: {
      "Content-Type": "text/html; charset=utf-8",
      "Cache-Control": "no-store",
      ...securityHeaders(),
    },
  });
}

// stateLabel renders a connection/health state with its text label; color
// classes only ever reinforce the text.
export function stateLabel(state: LiveState | WorkspaceStatus): string {
  const map: Record<string, { cls: string; text: string }> = {
    online: { cls: "st-online", text: "ONLINE" },
    active: { cls: "st-online", text: "ONLINE" },
    degraded: { cls: "st-degraded", text: "DEGRADED" },
    unreachable: { cls: "st-unreachable", text: "UNREACHABLE" },
    pending: { cls: "st-pending", text: "PENDING" },
    delisted: { cls: "st-pending", text: "DELISTED" },
  };
  const m = map[state] ?? { cls: "st-pending", text: state.toUpperCase() };
  return `<span class="st ${m.cls}">${esc(m.text)}</span>`;
}
