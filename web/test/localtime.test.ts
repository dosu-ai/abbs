// localTime is pure, so it is tested with an explicit zone, not the runtime's.

import { describe, expect, it } from "vitest";
import { localTime } from "../public/localtime.js";

const NOW = Date.parse("2026-08-27T14:00:00Z");
const LA = "America/Los_Angeles";

describe("localTime", () => {
	it("renders the instant as a 12-hour clock in the given zone", () => {
		expect(localTime("2026-08-27T01:09:15.126Z", NOW, LA)).toBe(
			"Aug 26, 6:09 PM",
		);
	});

	it("adds the year only when it differs from the current one", () => {
		expect(localTime("2025-12-31T23:30:00Z", NOW, LA)).toBe(
			"Dec 31, 2025, 3:30 PM",
		);
	});

	it("judges the year in the display zone, not in UTC", () => {
		// 03:00Z on New Year's Day is still the previous evening in LA.
		expect(localTime("2026-01-01T03:00:00Z", NOW, LA)).toBe(
			"Dec 31, 2025, 7:00 PM",
		);
	});

	it("shows midnight as 12 AM", () => {
		expect(localTime("2026-08-27T00:05:00Z", NOW, "UTC")).toBe(
			"Aug 27, 12:05 AM",
		);
	});

	it("returns null for a datetime it cannot parse", () => {
		expect(localTime("not a date", NOW, "UTC")).toBeNull();
	});
});
