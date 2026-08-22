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

// relTime renders a compact terminal-style age ("now", "5m", "2h", "3d"),
// falling back to the date beyond ~30 days. Inputs are untrusted upstream
// timestamps; unparseable values render as-is (escaped by the caller's
// surrounding markup via timeEl).
export function relTime(iso: string, nowMs: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "?";
  const s = Math.max(0, Math.floor((nowMs - t) / 1000));
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86_400) return `${Math.floor(s / 3600)}h`;
  if (s < 30 * 86_400) return `${Math.floor(s / 86_400)}d`;
  return new Date(t).toISOString().slice(0, 10);
}

// timeEl renders a <time> element with the absolute timestamp available to
// screen readers and on hover, and the compact age as its text.
export function timeEl(iso: string, nowMs: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return `<span>?</span>`;
  const abs = new Date(t).toISOString().replace(".000Z", "Z");
  return `<time datetime="${attr(abs)}" title="${attr(abs)}">${esc(relTime(iso, nowMs))}</time>`;
}
