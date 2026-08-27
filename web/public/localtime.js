// @ts-check
// Local-clock rendering for the timestamps the server prints in UTC. The
// server side stays deterministic (stable HTML and ETags, readable without
// script); this only decides what the viewer's browser shows. Pure so it
// can be tested with an explicit zone — app.js passes the browser's.

/**
 * localTime renders an ISO instant as e.g. "Aug 27, 1:09 AM" in `timeZone`
 * (the browser's zone when omitted), adding the year only when it differs
 * from `nowMs`'s year in that same zone. Returns null when `iso` does not
 * parse, so the caller can leave the server's text alone.
 *
 * @param {string} iso
 * @param {number} nowMs
 * @param {string} [timeZone]
 * @returns {string | null}
 */
export function localTime(iso, nowMs, timeZone) {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return null;
  const when = new Date(t);
  const parts = new Intl.DateTimeFormat("en-US", {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
    hour12: true,
    timeZone,
  }).formatToParts(when);
  /** @param {Intl.DateTimeFormatPartTypes} type */
  const part = (type) => parts.find((p) => p.type === type)?.value ?? "";
  const thisYear = new Intl.DateTimeFormat("en-US", { year: "numeric", timeZone })
    .format(new Date(nowMs));
  // Assembled from parts rather than taken from format(): the joiner between
  // date and time varies by ICU version ("," in some engines, "at" in
  // others), and the layout counts on a fixed width.
  const year = part("year") === thisYear ? "" : `, ${part("year")}`;
  return `${part("month")} ${part("day")}${year}, ${part("hour")}:${part("minute")} ${part("dayPeriod")}`;
}
