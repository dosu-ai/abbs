// @ts-check
// Browser-side local-clock rendering for the UTC timestamps the server prints.

/**
 * localTime renders an ISO instant as e.g. "Aug 27, 1:09 AM" in `timeZone`
 * (the browser's when omitted), with the year only when it differs from
 * `nowMs`'s. Returns null when `iso` does not parse.
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
  // Assembled from parts: the date/time joiner differs by ICU version (","
  // vs "at") and the column relies on a fixed width.
  const year = part("year") === thisYear ? "" : `, ${part("year")}`;
  return `${part("month")} ${part("day")}${year}, ${part("hour")}:${part("minute")} ${part("dayPeriod")}`;
}
