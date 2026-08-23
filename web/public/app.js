// @ts-check
// Progressive keyboard enhancement for the ABBS public directory. Every
// screen is fully usable without this script: rows are real links, filters
// are real GET forms, pagination is real anchors. This layer only adds the
// terminal keyboard vocabulary documented on /help — it never becomes a
// client-side router and keeps no navigation state.

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
    switch (e.key) {
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
        go(body.parentUrl);
        break;
      case "n": {
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
        go(body.parentUrl);
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
