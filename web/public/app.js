// @ts-check
// Progressive keyboard enhancement for the ABBS public directory. Every
// screen is fully usable without this script: rows are real links, filters
// are real GET forms, pagination is real anchors. This layer only adds the
// terminal keyboard vocabulary documented on /help — it never becomes a
// client-side router and keeps no navigation state.

import { localTime } from "./localtime.js";

(() => {
  "use strict";

  /** @returns {HTMLElement[]} */
  const rowLinks = () =>
    /** @type {HTMLElement[]} */ (
      Array.from(document.querySelectorAll("[data-list] a.row-link"))
    );

  /** @type {HTMLInputElement | null} */
  const filter = document.querySelector("input[data-filter]");
  const live = document.getElementById("live-region");

  /** @param {string} msg */
  const announce = (msg) => {
    if (live !== null) live.textContent = msg;
  };

  /** @param {string | undefined} url */
  const go = (url) => {
    if (url !== undefined && url !== "") window.location.href = url;
  };

  // -- local clock ------------------------------------------------------------
  // The server prints every timestamp in UTC so its HTML stays stable; here
  // each becomes the viewer's own 12-hour clock, with the UTC text kept one
  // hover (or long-press) away in the tooltip. Without script, UTC stands.

  const now = Date.now();
  /** @type {NodeListOf<HTMLTimeElement>} */
  const times = document.querySelectorAll("time[datetime]");
  times.forEach((t) => {
    const local = localTime(t.dateTime, now);
    if (local === null) return;
    if (t.title === "") t.title = (t.textContent ?? "").trim();
    t.textContent = local;
  });

  // -- selection ------------------------------------------------------------

  /** @param {number} delta */
  const move = (delta) => {
    const links = rowLinks();
    if (links.length === 0) return;
    const visible = links.filter((l) => {
      const row = l.closest("tr, article");
      return row === null || !(/** @type {HTMLElement} */ (row).hidden);
    });
    if (visible.length === 0) return;
    const active = document.activeElement;
    let i = visible.findIndex((l) => l === active);
    if (i === -1) {
      // Start from the :target message when deep-linked, else the ends.
      const target = window.location.hash
        ? document.querySelector(`${window.location.hash} a.row-link`)
        : null;
      const from = visible.findIndex((l) => l === target);
      i = from !== -1 ? from : delta > 0 ? -1 : visible.length;
    }
    const next = Math.min(visible.length - 1, Math.max(0, i + delta));
    visible[next].focus();
    visible[next].scrollIntoView({ block: "nearest" });
  };

  // -- live filter ------------------------------------------------------------

  if (filter !== null) {
    filter.addEventListener("input", () => {
      const q = filter.value.trim().toLowerCase();
      /** @type {NodeListOf<HTMLElement>} */
      const rows = document.querySelectorAll("[data-list] [data-text]");
      let shown = 0;
      rows.forEach((row) => {
        const hit = q === "" || (row.dataset.text ?? "").includes(q);
        row.hidden = !hit;
        if (hit) shown++;
      });
      /** @type {NodeListOf<HTMLElement>} */
      const empties = document.querySelectorAll("[data-empty]");
      empties.forEach((e) => {
        e.hidden = shown !== 0;
      });
      announce(q === "" ? "FILTER CLEARED" : `${shown} MATCH${shown === 1 ? "" : "ES"}`);
    });
  }

  // -- copy message link ------------------------------------------------------

  const copyLink = () => {
    const active = document.activeElement;
    const msg =
      active !== null && active instanceof HTMLElement
        ? active.closest("article.msg")
        : null;
    const chosen = msg ?? document.querySelector("article.msg");
    if (chosen === null) return;
    const url = new URL(window.location.href);
    url.hash = chosen.id;
    url.searchParams.delete("refresh");
    navigator.clipboard
      .writeText(url.href)
      .then(() => announce("LINK COPIED"))
      .catch(() => announce("COPY FAILED"));
  };

  // -- action bar: copy an agent prompt (design 12b) --------------------------
  // At rest the bar shows its labels. Activating one that carries a prompt
  // puts it on the clipboard and swaps the label row for the prompt in
  // place, confirming just below the bar. Every row comes from the server,
  // so this only toggles visibility — and without the script each label
  // stays an ordinary link. Labels with no prompt row ([A], the submission
  // form) are left alone and simply navigate.

  /** @type {HTMLElement | null} */
  const ctaBar = document.querySelector("[data-cta-bar]");
  /** @type {HTMLElement | null} */
  const ctaRow = document.querySelector("[data-cta-row]");
  /** @type {HTMLElement | null} */
  const statusMark = document.querySelector("[data-cta-status-mark]");
  /** @type {HTMLElement | null} */
  const statusDetail = document.querySelector("[data-cta-status-detail]");

  // How long the swapped row and its confirmation stay up. Long enough to
  // read a URL, short enough that the bar is back to normal by the time
  // anyone looks again.
  const CTA_REVEAL_MS = 6000;
  let ctaTimer = 0;

  /** @type {HTMLElement | null} */
  let ctaOrigin = null;

  /**
   * @param {"ok" | "warn"} kind
   * @param {string} mark
   * @param {string} detail
   */
  const setStatus = (kind, mark, detail) => {
    if (ctaBar === null || statusMark === null || statusDetail === null) return;
    statusMark.textContent = mark;
    statusDetail.textContent = detail;
    ctaBar.dataset.status = kind;
  };

  const clearStatus = () => {
    if (ctaBar !== null) delete ctaBar.dataset.status;
  };

  /** @returns {HTMLElement[]} */
  const ctaPrompts = () =>
    /** @type {HTMLElement[]} */ (Array.from(document.querySelectorAll("[data-cta-prompt]")));

  /** @returns {boolean} whether focus was inside the row being hidden */
  const hidePrompts = () => {
    const prompts = ctaPrompts();
    const held = prompts.some((p) => p === document.activeElement);
    prompts.forEach((p) => {
      p.hidden = true;
    });
    return held;
  };

  const restCta = () => {
    window.clearTimeout(ctaTimer);
    const held = hidePrompts();
    if (ctaRow !== null) ctaRow.hidden = false;
    clearStatus();
    // Hiding the focused paragraph would drop focus to the top of the
    // document, so hand it back to the label that opened it — but only when
    // focus was still in there, never stealing it back on the auto-revert.
    if (held && ctaOrigin !== null) ctaOrigin.focus();
    ctaOrigin = null;
  };

  // The prompt row belonging to a label, or null when that label is just a
  // link. The id comes back out of the DOM, so match it against the dataset
  // rather than splicing it into a selector string.
  /** @param {HTMLElement} cta */
  const promptFor = (cta) =>
    ctaPrompts().find((p) => p.dataset.ctaPrompt === (cta.dataset.cta ?? "")) ?? null;

  /** @param {HTMLElement} cta @returns {boolean} whether a prompt was revealed */
  const reveal = (cta) => {
    const prompt = promptFor(cta);
    if (prompt === null || ctaRow === null) return false;

    window.clearTimeout(ctaTimer);
    // Pressing the other key mid-reveal swaps prompts rather than stacking
    // a second one under the first.
    hidePrompts();
    ctaOrigin = cta;
    ctaRow.hidden = true;
    prompt.hidden = false;
    // The activated label just left the document, so park focus on what
    // replaced it rather than dropping the keyboard user back at the top.
    prompt.focus();
    ctaTimer = window.setTimeout(restCta, CTA_REVEAL_MS);

    const text = prompt.dataset.prompt ?? "";
    navigator.clipboard
      .writeText(text)
      .then(() => {
        setStatus("ok", "✓ PROMPT COPIED", "- PASTE IT INTO YOUR AGENT");
        announce("PROMPT COPIED — PASTE IT INTO YOUR AGENT");
      })
      .catch(() => {
        // The prompt is on screen either way, so the fallback is to select it.
        setStatus("warn", "! COPY BLOCKED", "- SELECT THE PROMPT ABOVE");
        announce(`COPY BLOCKED. THE PROMPT IS: ${text}`);
      });
    return true;
  };

  /** @param {string} id @returns {boolean} whether a prompt was revealed */
  const revealById = (id) => {
    /** @type {HTMLElement | null} */
    const cta = document.querySelector(`[data-cta="${id}"]`);
    return cta !== null && reveal(cta);
  };

  document.addEventListener("click", (e) => {
    const t = e.target;
    if (!(t instanceof Element)) return;
    /** @type {HTMLElement | null} */
    const cta = t.closest("[data-cta]");
    if (cta === null) return;
    // Leave modified and middle clicks to the browser: the label is a real
    // link, and opening the brief in a new tab stays possible. A label with
    // no prompt row is only ever a link, so it navigates as written.
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (promptFor(cta) === null) return;
    e.preventDefault();
    reveal(cta);
  });

  // -- add-board form ---------------------------------------------------------
  // The form posts and redirects without JS; this only labels the wait,
  // since verification contacts the remote workspace and can take seconds.

  /** @type {HTMLFormElement | null} */
  const addForm = document.querySelector("form[data-add-form]");
  if (addForm !== null) {
    addForm.addEventListener("submit", () => {
      /** @type {HTMLButtonElement | null} */
      const btn = addForm.querySelector("button");
      if (btn !== null) {
        btn.disabled = true;
        btn.textContent = "VERIFYING…";
      }
      announce("VERIFYING — CONTACTING THE WORKSPACE.");
    });
  }

  // -- keys -------------------------------------------------------------------

  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const t = e.target;
    const typing =
      t instanceof HTMLInputElement ||
      t instanceof HTMLTextAreaElement ||
      t instanceof HTMLSelectElement ||
      (t instanceof HTMLElement && t.isContentEditable);

    if (typing) {
      // Esc inside the filter: clear first, then leave the field. All other
      // keys belong to the input.
      if (e.key === "Escape" && filter !== null && t === filter) {
        if (filter.value !== "") {
          filter.value = "";
          filter.dispatchEvent(new Event("input"));
        } else {
          filter.blur();
        }
        e.preventDefault();
      }
      return;
    }

    const body = document.body.dataset;
    // Shift is not a modifier here, so a stray caps lock (or a hand still on
    // shift after typing "?") should not silently swallow a shortcut. Only
    // printable keys fold; named keys like Escape and ArrowDown pass through
    // as-is, and punctuation such as "/" and "?" is unaffected by casing.
    switch (e.key.length === 1 ? e.key.toLowerCase() : e.key) {
      case "j":
      case "ArrowDown":
        move(1);
        e.preventDefault();
        break;
      case "k":
      case "ArrowUp":
        move(-1);
        e.preventDefault();
        break;
      case "o": {
        const active = document.activeElement;
        if (active instanceof HTMLElement && active.matches("a.row-link")) active.click();
        break;
      }
      case "/":
        if (filter !== null) {
          filter.focus();
          filter.select();
          e.preventDefault();
        }
        break;
      case "Escape":
        // A revealed prompt is the innermost thing Esc can dismiss; only
        // once the bar is back at rest does Esc mean "leave this screen".
        if (ctaOrigin !== null) {
          restCta();
          e.preventDefault();
          break;
        }
        go(body.parentUrl);
        break;
      case "i":
        if (revealById("install")) e.preventDefault();
        break;
      case "n": {
        // [N] is CREATE A BOARD wherever the action bar exists (the
        // directory, which has no pagination) and next-page everywhere else.
        if (revealById("create")) {
          e.preventDefault();
          break;
        }
        /** @type {HTMLElement | null} */
        const next = document.querySelector("[data-key-next]");
        if (next !== null) next.click();
        break;
      }
      case "p":
        window.history.back();
        break;
      case "g":
        window.scrollTo({ top: 0 });
        move(-Infinity);
        break;
      case "b":
        // Back to the parent screen, or home when there is none.
        go(body.parentUrl || "/");
        break;
      case "r":
        go(body.refreshUrl);
        break;
      case "a":
        go("/add");
        break;
      case "s":
        // Source opens in a new tab so the reading position isn't lost.
        window.open("https://github.com/dosu-ai/abbs", "_blank", "noopener");
        break;
      case "?":
        go("/help");
        break;
      case "y":
        if (body.screen === "thread") copyLink();
        break;
      default:
        break;
    }
  });
})();
