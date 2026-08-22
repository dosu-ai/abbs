// The renderer is the wall between untrusted message content and visitors'
// browsers: attack corpus first, features second.

import { describe, expect, it } from "vitest";
import { renderMarkdown } from "../src/markdown";

describe("attack corpus", () => {
  it("escapes raw HTML — script tags never survive", () => {
    const out = renderMarkdown(`<script>alert(1)</script>`);
    expect(out).not.toContain("<script");
    expect(out).toContain("&lt;script&gt;");
  });

  it("escapes event-handler and iframe injection", () => {
    const out = renderMarkdown(`<img src=x onerror=alert(1)> <iframe src="https://x"></iframe>`);
    expect(out).not.toContain("<img");
    expect(out).not.toContain("<iframe");
    // The attack text survives only as escaped, inert text.
    expect(out).toContain("&lt;img src=x onerror=alert(1)&gt;");
    // The autolinker must not swallow neighboring entities into an href.
    expect(out).not.toMatch(/href="[^"]*(?:quot|lt|gt)/);
  });

  it("rejects javascript: links and renders them inert", () => {
    const out = renderMarkdown(`[click](javascript:alert(1))`);
    expect(out).not.toContain("href");
    expect(out).toContain("javascript:alert(1)"); // literal, escaped text
  });

  it("rejects data: and other non-http schemes", () => {
    for (const url of ["data:text/html,x", "vbscript:x", "file:///etc/passwd", "ftp://x"]) {
      expect(renderMarkdown(`[x](${url})`)).not.toContain("<a ");
    }
  });

  it("never renders an <img>, even for image syntax", () => {
    const out = renderMarkdown(`![alt text](https://tracker.example/pixel.png)`);
    expect(out).not.toContain("<img");
    expect(out).toContain(`<a href="https://tracker.example/pixel.png"`);
    expect(out).toContain("[image:");
  });

  it("escapes quotes in URLs so attributes cannot break out", () => {
    const out = renderMarkdown(`[x](https://e.example/"onmouseover="alert(1))`);
    expect(out).not.toContain(`"onmouseover=`);
  });

  it("adds the full rel set to every user-authored link", () => {
    const out = renderMarkdown(`[x](https://a.example) and https://b.example`);
    const links = out.match(/<a /g) ?? [];
    const rels = out.match(/rel="noopener noreferrer nofollow ugc"/g) ?? [];
    expect(links.length).toBe(2);
    expect(rels.length).toBe(2);
  });

  it("keeps markdown inside code blocks inert and escaped", () => {
    const out = renderMarkdown("```\n<b>[x](https://a.example)</b>\n```");
    expect(out).toContain("&lt;b&gt;[x](https://a.example)&lt;/b&gt;");
    expect(out).not.toContain("<a ");
  });

  it("strips NUL so the slot sentinel cannot be forged", () => {
    const out = renderMarkdown("a\u00000\u0000b `code`");
    expect(out).not.toContain("\u0000");
    expect(out).toContain("<code>code</code>");
  });

  it("headings never join the page outline", () => {
    const out = renderMarkdown("# huge heading");
    expect(out).not.toMatch(/<h\d/);
    expect(out).toContain("<strong>huge heading</strong>");
  });
});

describe("features", () => {
  it("renders paragraphs and line breaks", () => {
    expect(renderMarkdown("one\ntwo\n\nthree")).toBe("<p>one<br>two</p>\n<p>three</p>");
  });

  it("renders emphasis", () => {
    const out = renderMarkdown("**bold** and *italic*");
    expect(out).toContain("<strong>bold</strong>");
    expect(out).toContain("<em>italic</em>");
  });

  it("renders inline code without formatting inside", () => {
    expect(renderMarkdown("`**not bold**`")).toContain("<code>**not bold**</code>");
  });

  it("renders links with query strings intact", () => {
    const out = renderMarkdown("[docs](https://a.example/p?x=1&y=2)");
    expect(out).toContain(`href="https://a.example/p?x=1&amp;y=2"`);
    expect(out).toContain(">docs</a>");
  });

  it("autolinks bare URLs, leaving trailing punctuation out", () => {
    const out = renderMarkdown("see https://a.example/x.");
    expect(out).toContain(`href="https://a.example/x"`);
    expect(out).toMatch(/<\/a>\./);
  });

  it("renders unordered and ordered lists", () => {
    expect(renderMarkdown("- a\n- b")).toBe("<ul><li>a</li><li>b</li></ul>");
    expect(renderMarkdown("1. a\n2. b")).toBe("<ol><li>a</li><li>b</li></ol>");
  });

  it("renders blockquotes", () => {
    expect(renderMarkdown("> quoted\n> lines")).toBe(
      "<blockquote><p>quoted<br>lines</p></blockquote>",
    );
  });

  it("highlights @mentions", () => {
    expect(renderMarkdown("ping @ada.bot")).toContain(`<span class="mention">@ada.bot</span>`);
  });

  it("survives an unclosed fence", () => {
    expect(renderMarkdown("```\ncode forever")).toContain("<pre><code>code forever</code></pre>");
  });

  it("renders empty content to nothing", () => {
    expect(renderMarkdown("")).toBe("");
  });
});
