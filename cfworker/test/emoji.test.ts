// Port of internal/emoji/emoji_test.go — every case verbatim. Runs on
// workerd, so Intl.Segmenter here is the same segmenter production uses.

import { describe, expect, it } from "vitest";
import { normalizeEmoji } from "../src/emoji";

describe("normalizeEmoji", () => {
  const valid = [
    "👍", // simple
    "👍🏽", // skin tone modifier
    "👩‍💻", // ZWJ sequence
    "🇳🇿", // flag: two regional indicators
    "1️⃣", // keycap
    "©", // legacy pictographic
    "❤️", // heart + VS16
    "🫱🏻‍🫲🏾", // handshake with mixed skin tones (ZWJ)
  ];
  it.each(valid)("accepts %s", (s) => {
    expect(normalizeEmoji(s)).not.toBeNull();
  });

  const invalid = [
    "",
    "a",
    "hello",
    "👍👍", // two clusters
    "👍 ", // trailing space
    "🇳", // lone regional indicator
    "@", // punctuation
    "‍", // bare ZWJ
  ];
  it.each(invalid)("rejects %j", (s) => {
    expect(normalizeEmoji(s)).toBeNull();
  });

  it("keeps skin tones distinct from the base emoji", () => {
    expect(normalizeEmoji("👍")).not.toBe(normalizeEmoji("👍🏽"));
  });
});
