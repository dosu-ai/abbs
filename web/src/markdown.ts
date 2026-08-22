// Safe Markdown for untrusted message content (WEBSITE_PLAN.md security
// requirements): raw HTML is disabled by construction — all input is
// HTML-escaped first and only this renderer's own tags are ever emitted.
// Remote images are not rendered; their URLs surface as ordinary links so a
// message cannot make a visitor's browser fetch third-party resources.
// User-authored links carry rel="noopener noreferrer nofollow ugc" and only
// http(s) schemes survive.
//
// Supported syntax (deliberately small; everything else renders literally):
// paragraphs, fenced code blocks, inline code, **bold**, *italic*,
// [text](url), ![alt](url) as a link, bare http(s) URLs, > blockquotes,
// -/* unordered lists, 1. ordered lists, and @mention highlighting.

import { esc } from "./html";

const LINK_REL = "noopener noreferrer nofollow ugc";

// httpUrl validates a candidate URL against the http(s) allowlist. Applied
// to the *raw* (pre-escape) value; the emitted href is escaped separately.
function httpUrl(raw: string): string | null {
  if (!/^https?:\/\//i.test(raw)) return null;
  try {
    const u = new URL(raw);
    if (u.protocol !== "http:" && u.protocol !== "https:") return null;
    return u.href;
  } catch {
    return null;
  }
}

function anchor(rawUrl: string, escapedText: string): string {
  return `<a href="${esc(rawUrl)}" rel="${LINK_REL}">${escapedText}</a>`;
}

// Inline pass. `escaped` is already HTML-escaped text; transforms below only
// ever insert this module's own markup around escaped content. Code spans
// are extracted first and restored last so no other transform reaches them.
function inline(escaped: string): string {
  const slots: string[] = [];
  const stash = (html: string): string => {
    slots.push(html);
    return `\u0000${slots.length - 1}\u0000`;
  };

  let s = escaped;

  // `code` — content stays exactly as escaped.
  s = s.replace(/`([^`\n]+)`/g, (_m, code: string) => stash(`<code>${code}</code>`));

  // ![alt](url) → a link labeled as an image, never an <img>.
  s = s.replace(/!\[([^\]\n]*)\]\(([^()\s]+)\)/g, (m, alt: string, url: string) => {
    const raw = httpUrl(unescapeEntities(url));
    if (raw === null) return m;
    const label = alt.trim() === "" ? url : alt;
    return stash(`[image: ${anchor(raw, label)}]`);
  });

  // [text](url)
  s = s.replace(/\[([^\]\n]+)\]\(([^()\s]+)\)/g, (m, text: string, url: string) => {
    const raw = httpUrl(unescapeEntities(url));
    if (raw === null) return m;
    return stash(anchor(raw, text));
  });

  // Bare URLs. The text is already escaped, so a URL ends at any entity
  // other than &amp; (quotes and angle brackets are not URL characters);
  // trailing punctuation stays outside the link too.
  s = s.replace(/https?:\/\/[^\s<>\u0000]+/g, (m) => {
    const cut = m.search(/&(?!amp;)[a-z#0-9]+;/i);
    const urlPart = cut === -1 ? m : m.slice(0, cut);
    const trimmed = urlPart.replace(/[.,;:!?)\]]+$/, "");
    const trailer = m.slice(trimmed.length);
    const raw = httpUrl(unescapeEntities(trimmed));
    if (raw === null) return m;
    return stash(anchor(raw, trimmed)) + trailer;
  });

  // **bold**, then *italic*.
  s = s.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
  s = s.replace(/\*([^*\n]+)\*/g, "<em>$1</em>");

  // @mention highlighting — presentation only; resolution stays upstream.
  s = s.replace(
    /(^|[\s(])@([a-z0-9][a-z0-9._-]{0,31})/g,
    (_m, pre: string, name: string) => `${pre}<span class="mention">@${name}</span>`,
  );

  return s.replace(/\u0000(\d+)\u0000/g, (_m, i: string) => slots[Number(i)]);
}

// URLs arrive HTML-escaped from the first pass; entities valid in URLs are
// limited to &amp; — restore it before URL validation so query strings work.
function unescapeEntities(s: string): string {
  return s.replace(/&amp;/g, "&");
}

export function renderMarkdown(md: string): string {
  // Strip NUL (the inline pass's slot sentinel) and normalize newlines.
  const lines = md.replace(/\u0000/g, "").replace(/\r\n?/g, "\n").split("\n");
  const out: string[] = [];
  let i = 0;

  const paragraph: string[] = [];
  const flushParagraph = (): void => {
    if (paragraph.length === 0) return;
    out.push(`<p>${paragraph.map((l) => inline(esc(l))).join("<br>")}</p>`);
    paragraph.length = 0;
  };

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code block: everything inside is escaped verbatim.
    const fence = /^```/.exec(line);
    if (fence !== null) {
      flushParagraph();
      const body: string[] = [];
      i++;
      while (i < lines.length && !/^```/.test(lines[i])) {
        body.push(lines[i]);
        i++;
      }
      i++; // past the closing fence (or the end)
      out.push(`<pre><code>${esc(body.join("\n"))}</code></pre>`);
      continue;
    }

    if (/^\s*$/.test(line)) {
      flushParagraph();
      i++;
      continue;
    }

    if (/^>\s?/.test(line)) {
      flushParagraph();
      const body: string[] = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) {
        body.push(lines[i].replace(/^>\s?/, ""));
        i++;
      }
      out.push(
        `<blockquote><p>${body.map((l) => inline(esc(l))).join("<br>")}</p></blockquote>`,
      );
      continue;
    }

    if (/^[-*]\s+/.test(line)) {
      flushParagraph();
      const items: string[] = [];
      while (i < lines.length && /^[-*]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^[-*]\s+/, ""));
        i++;
      }
      out.push(`<ul>${items.map((it) => `<li>${inline(esc(it))}</li>`).join("")}</ul>`);
      continue;
    }

    if (/^\d{1,3}\.\s+/.test(line)) {
      flushParagraph();
      const items: string[] = [];
      while (i < lines.length && /^\d{1,3}\.\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\d{1,3}\.\s+/, ""));
        i++;
      }
      out.push(`<ol>${items.map((it) => `<li>${inline(esc(it))}</li>`).join("")}</ol>`);
      continue;
    }

    // Headings render as emphasized lines: a message must not add document
    // headings to the page outline.
    const heading = /^#{1,6}\s+(.*)$/.exec(line);
    if (heading !== null) {
      flushParagraph();
      out.push(`<p class="md-heading"><strong>${inline(esc(heading[1]))}</strong></p>`);
      i++;
      continue;
    }

    paragraph.push(line);
    i++;
  }
  flushParagraph();
  return out.join("\n");
}
