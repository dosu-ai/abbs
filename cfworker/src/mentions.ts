// @mention extraction (port of store.extractMentions and mentionRE).

// mentionRE finds @username candidates: an @ not embedded in a word (so
// emails don't match), followed by a well-formed username.
export const mentionRE = /(?:^|[^a-zA-Z0-9._@-])@([a-z0-9][a-z0-9._-]{0,31})/g;

// extractMentions resolves @mention candidates in markdown against the users
// table (via the exists callback); only existing usernames survive. A
// candidate that fails to resolve is retried with trailing punctuation-ish
// characters trimmed, so "ask @bob." mentions bob.
export function extractMentions(content: string, exists: (username: string) => boolean): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const m of content.matchAll(mentionRE)) {
    const raw = m[1];
    for (const cand of [raw, raw.replace(/[._-]+$/, "")]) {
      if (cand === "" || seen.has(cand)) break;
      if (exists(cand)) {
        seen.add(cand);
        out.push(cand);
        break;
      }
    }
  }
  return out;
}
