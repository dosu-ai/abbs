// HTML escaping and tiny templating helpers. Every workspace-supplied value
// is untrusted input; nothing reaches a page unescaped.

const ESC_RE = /[&<>"']/g;
const ESC_MAP: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

export function esc(s: string): string {
  return s.replace(ESC_RE, (c) => ESC_MAP[c]);
}

// attr escapes a value for a double-quoted attribute position.
export function attr(s: string): string {
  return esc(s);
}

// Stable absolute timestamps keep server-rendered HTML and ETags deterministic.
export function absoluteTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "?";
  return new Date(t)
    .toISOString()
    .replace("T", " ")
    .replace(/\.000Z$/, " UTC")
    .replace(/Z$/, " UTC");
}

// timeEl renders the absolute timestamp as both semantic and visible text.
export function timeEl(iso: string, _nowMs?: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return `<span>?</span>`;
  const abs = new Date(t).toISOString().replace(".000Z", "Z");
  return `<time datetime="${attr(abs)}">${esc(absoluteTime(iso))}</time>`;
}
