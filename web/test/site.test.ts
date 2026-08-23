// Integration: the served website against two mocked conforming public
// workspaces and one dead one — the Phase 2 exit criterion is walking from
// the board directory to a real message with plain HTTP navigation.

import {
	createExecutionContext,
	env,
	waitOnExecutionContext,
} from "cloudflare:test";
import { beforeAll, describe, expect, it } from "vitest";
import worker from "../src/index";
import {
	JSON_HEADERS,
	THREAD_ID,
	pageBody,
	serverInfoBody,
	threadBody,
} from "./helpers";
import { seedWorkspace } from "./helpers";
import { fetchMock } from "./mock";

const MSG_1 = "0198aaaa-bbbb-7ccc-8ddd-eeeeffff1001";
const MSG_2 = "0198aaaa-bbbb-7ccc-8ddd-eeeeffff1002";
const MSG_3 = "0198aaaa-bbbb-7ccc-8ddd-eeeeffff1003";

// Dispatch straight into the worker's fetch handler — the same modules the
// tests import, so the upstream fetch seam and caches are shared.
async function site(path: string, init?: RequestInit): Promise<Response> {
	const ctx = createExecutionContext();
	const resp = await worker.fetch(
		new Request(`https://abbs.dev${path}`, init),
		env,
		ctx,
	);
	// Settle waitUntil work (opportunistic registry health writes).
	await waitOnExecutionContext(ctx);
	return resp;
}

beforeAll(async () => {
	fetchMock.activate();
	fetchMock.disableNetConnect();

	// Registry rows live for the whole file (writes in beforeAll persist
	// across the file's tests under isolated storage).
	await seedWorkspace({
		slug: "ws-one",
		name: "one-name",
		description: "agents building one",
	});
	await seedWorkspace({
		slug: "ws-two",
		name: "two-name",
		description: "experiments and reports",
	});
	await seedWorkspace({
		slug: "ws-down",
		baseUrl: "https://down.example",
		name: "down-name",
	});

	const one = fetchMock.get("https://ws-one.example");
	const two = fetchMock.get("https://ws-two.example");
	const down = fetchMock.get("https://down.example");

	// Discovery metadata matches the seeds so the opportunistic health
	// write-back never changes what later tests assert on.
	one
		.intercept({ path: "/v1/server" })
		.reply(
			200,
			serverInfoBody("one-name", { description: "agents building one" }),
			JSON_HEADERS,
		)
		.persist();
	two
		.intercept({ path: "/v1/server" })
		.reply(
			200,
			serverInfoBody("two-name", { description: "experiments and reports" }),
			JSON_HEADERS,
		)
		.persist();
	down
		.intercept({ path: "/v1/server" })
		.replyWithError(new Error("connect failed"))
		.persist();
	down
		.intercept({ path: "/v1/threads", query: { limit: "50" } })
		.replyWithError(new Error("connect failed"))
		.persist();
	down
		.intercept({ path: "/v1/threads" })
		.replyWithError(new Error("connect failed"))
		.persist();
	down
		.intercept({ path: "/v1/tags", query: { limit: "50" } })
		.replyWithError(new Error("connect failed"))
		.persist();

	const threads = [
		threadBody(),
		threadBody({
			id: "0198aaaa-bbbb-7ccc-8ddd-eeeeffff0009",
			title: "Release checklist for v1",
			tags: ["release"],
			creator: "buildbot",
		}),
	];
	one
		.intercept({ path: "/v1/threads", query: { limit: "50" } })
		.reply(200, pageBody(threads, "tok2"), JSON_HEADERS)
		.persist();
	one
		.intercept({ path: "/v1/threads" })
		.reply(200, pageBody(threads, "tok2"), JSON_HEADERS)
		.persist();
	one
		.intercept({ path: "/v1/tags", query: { limit: "50" } })
		.reply(
			200,
			pageBody([
				{ name: "api", thread_count: 2 },
				{ name: "release", thread_count: 1 },
			]),
			JSON_HEADERS,
		)
		.persist();
	one
		.intercept({ path: `/v1/threads/${THREAD_ID}` })
		.reply(200, JSON.stringify(threadBody()), JSON_HEADERS)
		.persist();

	const messages = [
		{
			id: MSG_1,
			thread_id: THREAD_ID,
			author: "ada",
			content:
				"The poll and **websocket** tails should be sequence-equivalent. See [the spec](https://spec.example/v1?a=1&b=2).",
			deleted: false,
			created_at: "2026-08-22T09:14:00Z",
			edited_at: null,
			seq: "188",
			reactions: [],
		},
		{
			id: MSG_2,
			thread_id: THREAD_ID,
			author: "buildbot",
			content:
				"Conformance is green for reconnect from the last committed cursor.",
			deleted: false,
			created_at: "2026-08-22T09:22:00Z",
			edited_at: "2026-08-22T09:25:00Z",
			seq: "193",
			reactions: [
				{ emoji: "👍", count: 2 },
				{ emoji: "👀", count: 1 },
			],
		},
		{
			id: MSG_3,
			thread_id: THREAD_ID,
			author: "lin",
			deleted: true,
			created_at: "2026-08-22T09:30:00Z",
			deleted_at: "2026-08-22T09:40:00Z",
			deleted_by: "mod",
			seq: "195",
			reactions: [],
		},
	];
	one
		.intercept({
			path: `/v1/threads/${THREAD_ID}/messages`,
			query: { limit: "50" },
		})
		.reply(200, pageBody(messages, "mtok"), JSON_HEADERS)
		.persist();
	one
		.intercept({ path: `/v1/threads/${THREAD_ID}/messages` })
		.reply(200, pageBody(messages, "mtok"), JSON_HEADERS)
		.persist();

	const users: Record<string, { kind: string; display_name?: string }> = {
		ada: { kind: "human", display_name: "Ada L." },
		buildbot: { kind: "agent" },
		lin: { kind: "human" },
		mod: { kind: "human" },
	};
	for (const [username, u] of Object.entries(users)) {
		one
			.intercept({ path: `/v1/users/${username}` })
			.reply(200, JSON.stringify({ username, ...u }), JSON_HEADERS)
			.persist();
	}
});

describe("board directory", () => {
	it("lists boards with live status labels and real links", async () => {
		const r = await site("/");
		expect(r.status).toBe(200);
		const html = await r.text();
		expect(html).toContain("ABBS PUBLIC DIRECTORY");
		expect(html).toContain("2 BOARDS ONLINE");
		expect(html).toContain(`href="/w/ws-one"`);
		expect(html).toContain(`href="/w/ws-two"`);
		expect(html).toContain("one-name");
		expect(html).toContain("agents building one");
		expect(html).toContain("ONLINE");
		expect(html).toContain("UNREACHABLE"); // the dead board is labeled, not hidden
		expect(html).toContain("SKIP TO CONTENT");
	});

	it("filters server-side via ?q=", async () => {
		const r = await site("/?q=experiments");
		const html = await r.text();
		expect(html).toContain("two-name");
		expect(html).not.toContain(`href="/w/ws-one"`);
	});

	it("merges CHECKED into STATUS, keeping the time readable without a mouse", async () => {
		// Seeded rows have never been checked; stamp one so both branches render.
		await env.DB.prepare(
			"UPDATE workspaces SET last_checked_at = ?1 WHERE slug = ?2",
		)
			.bind("2026-08-22T09:00:00Z", "ws-down")
			.run();
		const html = await (await site("/")).text();

		// The column is gone; the width it was spending goes to the description.
		expect(html).not.toContain(`<th scope="col">CHECKED</th>`);
		expect(html).toContain(`title="LAST CHECKED 2026-08-22 09:00:00 UTC"`);
		// A title attribute is a mouse affordance only — invisible to a screen
		// reader and unreachable on touch — so the time is real text as well,
		// and still machine-readable.
		expect(html).toContain(
			`<span class="visually-hidden">, last checked <time datetime="2026-08-22T09:00:00Z"`,
		);
		// A board with no check yet says so rather than rendering a bare dash.
		expect(html).toContain(
			`<span class="visually-hidden">, never checked</span>`,
		);

		const css = await (await site("/styles.css")).text();
		expect(css).toContain("table.list td.status[title]");
		expect(css).toContain("cursor: help");
	});

	it("floors the name column so a board name never breaks mid-word", async () => {
		const css = await (await site("/styles.css")).text();
		// The guard against a long unbreakable upstream token stays — it is the
		// reason the column could collapse in the first place, not a mistake.
		expect(css).toContain("overflow-wrap: anywhere");
		expect(css).toContain("table.list td.name");
		expect(css).toContain("min-width: 18ch");
		expect(css).toContain("table.list td.status");
	});

	it("sends the full security header set on HTML", async () => {
		const r = await site("/");
		expect(r.headers.get("Content-Security-Policy")).toContain(
			"default-src 'none'",
		);
		expect(r.headers.get("Content-Security-Policy")).toContain(
			"frame-ancestors 'none'",
		);
		expect(r.headers.get("X-Frame-Options")).toBe("DENY");
		expect(r.headers.get("X-Content-Type-Options")).toBe("nosniff");
		expect(r.headers.get("Referrer-Policy")).toBe("no-referrer");
	});
});

describe("workspace board", () => {
	it("renders threads in upstream order with tags, provenance, and paging", async () => {
		const r = await site("/w/ws-one");
		expect(r.status).toBe(200);
		const html = await r.text();
		expect(html).toContain("CONNECTED:");
		expect(html).toContain("Replace polling with websocket");
		expect(html).toContain("Release checklist for v1");
		expect(html).toContain("@ada");
		expect(html).toContain("[api 2]");
		expect(html).toContain(`href="/w/ws-one/t/${THREAD_ID}"`);
		expect(html).toContain("page=tok2"); // NEXT PAGE preserves the opaque token
	});

	it("404s an unknown board", async () => {
		const r = await site("/w/nope");
		expect(r.status).toBe(404);
		expect(await r.text()).toContain("404");
	});

	it("labels a dead workspace unreachable with a 504", async () => {
		const r = await site("/w/ws-down");
		expect(r.status).toBe(504);
		const html = await r.text();
		expect(html).toContain("CONNECTION FAILED");
		expect(html).toContain("UNREACHABLE");
	});
});

describe("thread reader", () => {
	it("renders messages with markdown, provenance, edits, tombstones, reactions", async () => {
		const r = await site(`/w/ws-one/t/${THREAD_ID}`);
		expect(r.status).toBe(200);
		const html = await r.text();
		// Safe markdown.
		expect(html).toContain("<strong>websocket</strong>");
		expect(html).toContain(`href="https://spec.example/v1?a=1&amp;b=2"`);
		expect(html).toContain(`rel="noopener noreferrer nofollow ugc"`);
		// Provenance.
		expect(html).toContain("@ada");
		expect(html).toContain("[HUMAN]");
		expect(html).toContain("[AGENT]");
		// Edit and delete state.
		expect(html).toContain("(edited)");
		expect(html).toContain("[message deleted");
		expect(html).toContain("@mod");
		// Reactions render as tallies with no action.
		expect(html).toContain("👍 2");
		expect(html).toContain("👀 1");
		// Stable message anchors and pagination.
		expect(html).toContain(`id="m-${MSG_1}"`);
		expect(html).toContain("page=mtok");
	});

	it("404s a malformed thread id without contacting the workspace", async () => {
		const r = await site("/w/ws-one/t/not-a-uuid");
		expect(r.status).toBe(404);
	});

	// On a phone there is no Esc key, and a visitor from search has no history
	// to go back through: the trail is the only way back up.
	it("links every breadcrumb ancestor back up the hierarchy", async () => {
		const html = await (await site(`/w/ws-one/t/${THREAD_ID}`)).text();
		expect(html).toContain(`<a class="crumb" href="/">ABBS</a>`);
		expect(html).toContain(`<a class="crumb" href="/w/ws-one">`);
		// The thread itself is the current page, so it is text, not a link.
		expect(html).toContain(`class="crumb crumb-current" aria-current="page"`);
	});

	it("links the board breadcrumb home", async () => {
		const html = await (await site("/w/ws-one")).text();
		expect(html).toContain(`<a class="crumb" href="/">ABBS</a>`);
	});
});

describe("mobile (design 8a)", () => {
	it("ships a narrow wordmark alongside the wide one for phones", async () => {
		const html = await (await site("/")).text();
		expect(html).toContain(`class="art art-wide"`);
		expect(html).toContain(`class="art art-compact"`);
		const css = await (await site("/styles.css")).text();
		expect(css).toContain("pre.art-compact");
	});

	it("offers a tap hint in place of the keyboard strip", async () => {
		const html = await (await site("/")).text();
		expect(html).toContain(`class="touch-hint"`);
		const css = await (await site("/styles.css")).text();
		// Keyed on pointer, not width: a narrow desktop window keeps its keys.
		expect(css).toContain("(hover: none) and (pointer: coarse)");
	});
});

describe("action bar (design 12b)", () => {
	it("offers all three actions at rest as real links", async () => {
		const html = await (await site("/")).text();
		expect(html).toContain("CONNECT AN AGENT");
		expect(html).toContain("CREATE A BOARD");
		expect(html).toContain("ADD YOUR BOARD");
		expect(html).toContain(`href="/install.md"`);
		expect(html).toContain(`href="/create.md"`);
		// [A] moved out of the filter row and into the bar as a third action.
		expect(html).toContain(`data-cta="add"`);
		expect(html).not.toContain(`class="add-board"`);
	});

	it("gives [A] no prompt row — it is a form to fill in, not work to hand off", async () => {
		const html = await (await site("/")).text();
		expect(html).toContain(`data-cta-prompt="install"`);
		expect(html).toContain(`data-cta-prompt="create"`);
		expect(html).not.toContain(`data-cta-prompt="add"`);
	});

	it("ships the prompt rows hidden, with absolute URLs an agent can act on", async () => {
		const html = await (await site("/")).text();
		expect(html).toContain(
			`data-prompt="Tell your agent to setup ABBS https://abbs.dev/install.md"`,
		);
		expect(html).toContain(
			`data-prompt="Tell your agent to create a new public board https://abbs.dev/create.md"`,
		);
		// Design 12b: hidden until the row swap, so the server renders both
		// states and the client only toggles them.
		expect(html).toContain(`data-cta-prompt="install"`);
		expect(html).toContain(`tabindex="-1" hidden`);
	});

	it("confirms the copy directly under the bar without shifting the page", async () => {
		const html = await (await site("/")).text();
		expect(html).toContain(`class="cta-status" data-cta-status`);
		const css = await (await site("/styles.css")).text();
		// Hidden by visibility, not display, so the line is always reserved.
		expect(css).toContain("visibility: hidden");
		expect(css).toContain(".cta-bar[data-status] .cta-status");
		const js = await (await site("/app.js")).text();
		expect(js).toContain("PROMPT COPIED");
	});

	it("advertises every action key in the keyboard strip", async () => {
		const html = await (await site("/")).text();
		expect(html).toContain("<kbd>I</kbd> INSTALL");
		expect(html).toContain("<kbd>N</kbd> NEW");
		expect(html).toContain("<kbd>A</kbd> ADD BOARD");
	});

	it("serves the install brief as actionable markdown", async () => {
		const r = await site("/install.md");
		expect(r.status).toBe(200);
		expect(r.headers.get("Content-Type")).toBe(
			"text/markdown; charset=utf-8",
		);
		expect(r.headers.get("Cache-Control")).toBe("public, max-age=300");
		expect(r.headers.get("X-Content-Type-Options")).toBe("nosniff");

		const body = await r.text();
		expect(body).toContain("https://board.abbs.dev");
		expect(body).toContain("https://oss.abbs.dev");
		expect(body).toContain("claude mcp add abbs -- abbs mcp");
		expect(body).toContain("<!-- abbs:onboarding -->");
		expect(body).toContain("https://abbs.dev/w/abbs/t/<thread_id>");
		expect(body).not.toContain("abbs_");
		expect(body).not.toContain("WORK IN-PRORGRESS");
		expect(body.split("\n").length).toBeLessThan(200);
	});

	it("leaves the create brief on its placeholder", async () => {
		const r = await site("/create.md");
		expect(r.status).toBe(200);
		expect(r.headers.get("Content-Type")).toBe(
			"text/markdown; charset=utf-8",
		);
		expect(await r.text()).toBe("WORK IN-PRORGRESS - TRY AGAIN LATER\n");
	});
});

describe("api", () => {
	it("lists registry workspaces", async () => {
		const r = await site("/api/workspaces");
		expect(r.status).toBe(200);
		const body = (await r.json()) as {
			items: { slug: string; base_url: string }[];
		};
		expect(body.items.map((w) => w.slug).sort()).toEqual([
			"ws-down",
			"ws-one",
			"ws-two",
		]);
	});

	it("passes through protocol-shaped thread pages", async () => {
		const r = await site("/api/workspaces/ws-one/threads");
		expect(r.status).toBe(200);
		expect(r.headers.get("Cache-Control")).toContain("max-age=30");
		const body = (await r.json()) as {
			items: { title: string }[];
			next_page: string;
		};
		expect(body.items[0].title).toBe("Replace polling with websocket");
		expect(body.next_page).toBe("tok2");
	});

	it("returns workspace detail with live upstream state", async () => {
		const r = await site("/api/workspaces/ws-down");
		expect(r.status).toBe(200);
		const body = (await r.json()) as {
			upstream: { state: string; error_code?: string };
		};
		expect(body.upstream.state).toBe("unreachable");
	});

	it("maps upstream transport failure to a 504 problem", async () => {
		const r = await site("/api/workspaces/ws-down/threads");
		expect(r.status).toBe(504);
		expect(r.headers.get("Content-Type")).toContain("application/problem+json");
		const body = (await r.json()) as { type: string };
		expect(body.type).toContain("upstream-unreachable");
	});

	it("rejects bad parameters with a validation problem", async () => {
		const r = await site("/api/workspaces/ws-one/threads?limit=0");
		expect(r.status).toBe(400);
		const body = (await r.json()) as { type: string };
		expect(body.type).toContain("validation");
	});

	it("404s unknown workspaces and endpoints", async () => {
		expect((await site("/api/workspaces/nope/threads")).status).toBe(404);
		expect((await site("/api/nope")).status).toBe(404);
	});
});

describe("the only mutation is registration", () => {
	it("rejects every other method and every other POST target", async () => {
		for (const method of ["PUT", "PATCH", "DELETE"]) {
			const r = await site("/api/workspaces", { method });
			expect(r.status).toBe(405);
			expect(r.headers.get("Allow")).toBe("GET, HEAD, POST");
		}
		for (const path of [
			"/api/workspaces/ws-one/threads",
			"/help",
			"/w/ws-one",
		]) {
			const r = await site(path, { method: "POST" });
			expect(r.status).toBe(405);
			expect(r.headers.get("Allow")).toBe("GET, HEAD");
		}
	});

	it("answers HEAD without a body", async () => {
		const r = await site("/help", { method: "HEAD" });
		expect(r.status).toBe(200);
		expect(await r.text()).toBe("");
	});
});

describe("navigation plumbing", () => {
	it("serves the help and add screens", async () => {
		const help = await site("/help");
		expect(help.status).toBe(200);
		const helpHtml = await help.text();
		expect(helpHtml).toContain("KEYBOARD");
		// The about copy moved from the directory to the top of /help.
		expect(helpHtml).toContain(
			"ABBS is a thread-based messaging protocol and server",
		);
		expect(helpHtml).toContain(
			"Agents connect, catch up from a cursor, post to durable threads, and disconnect.",
		);
		expect(helpHtml).toContain(
			"a persistent place to coordinate across runs, tools, and machines",
		);
		const add = await site("/add");
		expect(add.status).toBe(200);
		expect(await add.text()).toContain("directory_listing");
	});

	it("redirects trailing slashes to the canonical URL", async () => {
		const r = await site("/add/", { redirect: "manual" });
		expect(r.status).toBe(308);
		expect(r.headers.get("Location")).toBe("https://abbs.dev/add");
	});

	it("serves static assets from the same origin", async () => {
		const css = await site("/styles.css");
		expect(css.status).toBe(200);
		expect(await css.text()).toContain("IBM VGA");
		const js = await site("/app.js");
		expect(js.status).toBe(200);
	});

	it("404s unknown pages with the terminal 404 screen", async () => {
		const r = await site("/no-such-page");
		expect(r.status).toBe(404);
		expect(await r.text()).toContain("NO SUCH PAGE");
	});
});
