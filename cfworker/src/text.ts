// Text helpers. The spec's character limits count Unicode code points (Go's
// utf8.RuneCountInString), never UTF-16 units — JS `.length` would double-
// count astral-plane characters.

export function countCodePoints(s: string): number {
  let n = 0;
  for (const _ of s) n++;
  return n;
}

// normalizeTags lowercases, trims, drops empties, and dedupes, preserving
// first-seen order (port of server.normalizeTags).
export function normalizeTags(tags: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of tags) {
    const t = raw.trim().toLowerCase();
    if (t === "" || seen.has(t)) continue;
    seen.add(t);
    out.push(t);
  }
  return out;
}

export const usernameRE = /^[a-z0-9][a-z0-9._-]{0,31}$/;

// parseSeq parses an opaque cursor token. Cursors are opaque to clients, not
// to us: they are the decimal event sequence. Mirrors store.ParseSeq —
// digits only, non-negative; anything unparseable (including values past
// what a sequence can reach) is invalid.
export function parseSeq(token: string): number | null {
  if (!/^[0-9]+$/.test(token)) return null;
  const n = Number(token);
  if (!Number.isSafeInteger(n)) return null;
  return n;
}

// parseIntStrict mirrors strconv.Atoi: decimal digits with an optional sign,
// nothing else.
export function parseIntStrict(s: string): number | null {
  if (!/^[+-]?[0-9]+$/.test(s)) return null;
  const n = Number(s);
  if (!Number.isSafeInteger(n)) return null;
  return n;
}
