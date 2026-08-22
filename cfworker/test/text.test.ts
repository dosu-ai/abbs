// Code-point counting (the spec's limits count code points, not UTF-16
// units), tag normalization, and mention extraction including the
// trailing-punctuation retry.

import { describe, expect, it } from "vitest";
import { countCodePoints, normalizeTags, parseIntStrict, parseSeq } from "../src/text";
import { extractMentions } from "../src/mentions";

describe("countCodePoints", () => {
  it("counts astral-plane characters once, not twice", () => {
    expect("𝄞".length).toBe(2); // UTF-16 units — the trap
    expect(countCodePoints("𝄞")).toBe(1);
    expect(countCodePoints("👍")).toBe(1);
    expect(countCodePoints("é")).toBe(1);
    expect(countCodePoints("")).toBe(0);
    expect(countCodePoints("a𝄞b")).toBe(3);
    // 8001 astral characters must read as 8001, not 16002.
    expect(countCodePoints("😀".repeat(8001))).toBe(8001);
  });
});

describe("normalizeTags", () => {
  it("lowercases, trims, dedupes, preserves first-seen order", () => {
    expect(normalizeTags([" Foo ", "bar", "FOO", "", "  ", "baz", "bar"])).toEqual(["foo", "bar", "baz"]);
  });
});

describe("parseSeq / parseIntStrict", () => {
  it("accepts plain decimal sequences only", () => {
    expect(parseSeq("0")).toBe(0);
    expect(parseSeq("42")).toBe(42);
    expect(parseSeq("banana")).toBeNull();
    expect(parseSeq("-1")).toBeNull();
    expect(parseSeq("1.5")).toBeNull();
    expect(parseSeq("1e3")).toBeNull();
    expect(parseSeq("99999999999999999999")).toBeNull(); // past safe-integer precision
  });
  it("parses limits the way strconv.Atoi does", () => {
    expect(parseIntStrict("30")).toBe(30);
    expect(parseIntStrict("-1")).toBe(-1);
    expect(parseIntStrict("12abc")).toBeNull(); // parseInt would say 12
    expect(parseIntStrict("")).toBeNull();
  });
});

describe("extractMentions", () => {
  const users = new Set(["bob", "carol", "d.a-v_e"]);
  const exists = (u: string) => users.has(u);

  it("resolves plain mentions", () => {
    expect(extractMentions("hey @bob and @carol", exists)).toEqual(["bob", "carol"]);
  });
  it("retries with trailing punctuation trimmed: @bob. mentions bob", () => {
    expect(extractMentions("ask @bob.", exists)).toEqual(["bob"]);
  });
  it("does not mention from email addresses", () => {
    expect(extractMentions("mail@carol.example", exists)).toEqual([]);
  });
  it("drops candidates that are not users", () => {
    expect(extractMentions("ping @mallory", exists)).toEqual([]);
  });
  it("dedupes repeated mentions", () => {
    expect(extractMentions("@bob @bob @bob", exists)).toEqual(["bob"]);
  });
  it("keeps interior punctuation when the full candidate resolves", () => {
    expect(extractMentions("cc @d.a-v_e today", exists)).toEqual(["d.a-v_e"]);
  });
});
