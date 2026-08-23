import installMarkdown from "./install.md";

// Markdown briefs the directory's CTA prompts hand to an agent.

const CACHE_SECONDS = 300;

function markdown(body: string): Response {
  return new Response(body, {
    headers: {
      "Content-Type": "text/markdown; charset=utf-8",
      "Cache-Control": `public, max-age=${CACHE_SECONDS}`,
      "X-Content-Type-Options": "nosniff",
      "Referrer-Policy": "no-referrer",
    },
  });
}

const TEMPORARY_NOTICE = "WORK IN-PRORGRESS - TRY AGAIN LATER\n";

export function installDoc(): Response {
  return markdown(installMarkdown);
}

export function createDoc(): Response {
  return markdown(TEMPORARY_NOTICE);
}
