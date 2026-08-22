// Port of internal/emoji/emoji.go — reaction emoji validation per the spec:
// exactly one Unicode emoji, meaning one extended grapheme cluster whose
// base is an emoji. ZWJ sequences (👩‍💻), skin-tone modifiers (👍🏽), flags
// (🇳🇿), and keycaps (1️⃣) are single clusters of multiple code points —
// segmentation uses Intl.Segmenter, never a codepoint regex. The normalized
// (NFC) cluster is the canonical key, so visually identical sequences don't
// fragment tallies.
//
// The ranges below are Go's hand-approximated Extended_Pictographic
// RangeTable, ported verbatim — parity with the reference implementation
// beats abstract correctness here (\p{Extended_Pictographic} would drift).

const R16: ReadonlyArray<readonly [number, number]> = [
  [0x00a9, 0x00a9], // ©
  [0x00ae, 0x00ae], // ®
  [0x203c, 0x203c],
  [0x2049, 0x2049],
  [0x2122, 0x2122],
  [0x2139, 0x2139],
  [0x2194, 0x21aa],
  [0x231a, 0x231b],
  [0x2328, 0x2328],
  [0x23cf, 0x23cf],
  [0x23e9, 0x23fa],
  [0x24c2, 0x24c2],
  [0x25aa, 0x25ab],
  [0x25b6, 0x25b6],
  [0x25c0, 0x25c0],
  [0x25fb, 0x25fe],
  [0x2600, 0x27bf], // misc symbols, dingbats
  [0x2934, 0x2935],
  [0x2b05, 0x2b07],
  [0x2b1b, 0x2b1c],
  [0x2b50, 0x2b50],
  [0x2b55, 0x2b55],
  [0x3030, 0x3030],
  [0x303d, 0x303d],
  [0x3297, 0x3297],
  [0x3299, 0x3299],
];

const R32: ReadonlyArray<readonly [number, number]> = [
  [0x1f000, 0x1fbff], // the emoji planes: symbols, pictographs, ext-A/B
];

function isExtendedPictographic(cp: number): boolean {
  for (const [lo, hi] of cp < 0x10000 ? R16 : R32) {
    if (cp >= lo && cp <= hi) return true;
  }
  return false;
}

function isRegionalIndicator(cp: number): boolean {
  return cp >= 0x1f1e6 && cp <= 0x1f1ff;
}

const segmenter = new Intl.Segmenter("en", { granularity: "grapheme" });

function graphemeClusterCount(s: string): number {
  let n = 0;
  for (const _ of segmenter.segment(s)) n++;
  return n;
}

// normalizeEmoji validates s as exactly one emoji and returns the canonical
// (NFC-normalized) cluster to use as the storage key, or null when invalid.
export function normalizeEmoji(s: string): string | null {
  s = s.normalize("NFC");
  if (s === "" || graphemeClusterCount(s) !== 1) return null;
  const runes = [...s].map((c) => c.codePointAt(0)!);
  const base = runes[0];
  if (isRegionalIndicator(base)) {
    // Checked before the pictographic table (regional indicators live inside
    // its range): a flag is exactly two regional indicators — a lone one is
    // not an emoji.
    if (runes.length === 2 && isRegionalIndicator(runes[1])) return s;
    return null;
  }
  if (isExtendedPictographic(base)) return s;
  if (runes.length > 1 && runes[runes.length - 1] === 0x20e3) {
    // Keycap sequence: base char + (VS16) + U+20E3.
    return s;
  }
  return null;
}
