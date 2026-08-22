// Fragments shared across screens: error panels for upstream states and the
// not-found page.

import { esc } from "../html";
import { page, stateLabel } from "../layout";
import type { UpstreamErr } from "../upstream";
import { isUnreachable } from "../upstream";

// errorPanel renders a typed upstream failure as terminal copy. Never
// includes upstream response content — only our bounded code.
export function errorPanel(err: UpstreamErr): string {
  let msg: string;
  if (isUnreachable(err.code)) {
    msg = "CONNECTION FAILED — THE WORKSPACE DID NOT ANSWER.";
  } else if (err.code === "rate-limited") {
    msg =
      err.retryAfterSeconds !== undefined
        ? `THE WORKSPACE IS RATE LIMITING THE DIRECTORY. RETRY IN ${Math.min(err.retryAfterSeconds, 3600)}S.`
        : "THE WORKSPACE IS RATE LIMITING THE DIRECTORY. RETRY SHORTLY.";
  } else if (err.code === "not-public") {
    msg = "THIS WORKSPACE NO LONGER ADVERTISES PUBLIC VISIBILITY.";
  } else {
    msg = `THE WORKSPACE ANSWERED UNEXPECTEDLY (${err.code.toUpperCase()}).`;
  }
  return `<section class="panel panel-error" role="alert">
  <p>${esc(msg)}</p>
  <p>PRESS <kbd>R</kbd> TO RETRY OR <kbd>B</kbd> TO GO BACK.</p>
</section>`;
}

// errorStatus maps an upstream failure to the HTML/JSON response status used
// when it prevented the page's primary content from loading.
export function errorStatus(err: UpstreamErr): number {
  if (isUnreachable(err.code)) return 504;
  if (err.code === "not-found") return 404;
  if (err.code === "rate-limited") return 503;
  return 502;
}

export function notFoundPage(): Response {
  return page({
    title: "NOT FOUND",
    screen: "error",
    parentUrl: "/",
    headerLeft: `<h1>ABBS PUBLIC DIRECTORY</h1>`,
    headerRight: stateLabel("degraded"),
    main: `<section class="panel panel-error">
  <p>404 — NO SUCH PAGE.</p>
  <p><a href="/">RETURN TO THE BOARD DIRECTORY</a></p>
</section>`,
    keys: [
      { keys: ["B"], label: "BOARDS" },
      { keys: ["?"], label: "HELP" },
    ],
    status: 404,
  });
}
