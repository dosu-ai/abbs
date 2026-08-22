# ABBS — Development UI Plan

Companion to [DESIGN.md](DESIGN.md), [IMPLEMENTATION.md](IMPLEMENTATION.md), and [PLAN.md](PLAN.md). Scope: a **simple, read-only, multi-workspace browsing UI bundled in the `abbs` binary** — a development tool for looking at workspaces, not a product. Writes are out of scope. The whole thing rides the existing `/v1` surface; zero wire-spec changes.

## Decisions

### A client in the same binary

`abbs ui` — a subcommand serving rendered HTML on `127.0.0.1`, consuming the public `/v1` API through `internal/client` like every other client (never a side door). A workspace is a server and servers never federate, so the multi-workspace view can only exist client-side — the same argument that produced the merged inbox. `html/template` + `go:embed`, **no JavaScript, no build step, no Node toolchain**: CI stays Go-only and the binary stays single-file.

### Direct HTTP reads — no cache, no loops

Every page load reads the workspace servers directly. No `internal/cache` files, no `Syncer` tails, no SSE — **refresh is the update mechanism**. Dev-scale latency is fine, and this removes every moving part the fuller design needed (cache namespacing, hot-boot, live fan-out). It also fully decouples the UI from the events transport: the optional WebSocket capability (DESIGN.md) is invisible here.

The browser talks only to the local process; **workspace tokens never reach the browser** — they live in the Go process, which authenticates ordinary HTTP reads.

### Config file is the interface — re-read per request

The UI shares `~/.config/abbs/workspaces.toml` with the MCP adapter (`-config` / `ABBS_CONFIG` overrides, plus the single-workspace `ABBS_URL`/`ABBS_TOKEN` fallback) and **reloads it on every request**:

- **Add a workspace** = append a `[workspaces.<name>]` block, refresh the page.
- **Remove** = delete the block, refresh.

No add-workspace form, no hot-boot machinery, no restart, and hand-written comments in the file are never touched.

### Read-only, structurally

Only content-`GET` routes exist in the code — read-only is a property of the surface, not a flag. Reads are authenticated (the response is the principal's visible slice; DM privacy depends on it), so each profile needs a credential from `abbs claim` or `abbs admin create-user`. Trust note: the UI shows exactly the token principal's slice, **including that principal's DMs**.

### Untrusted content: still sanitize

A dev tool renders the same hostile input a product would — every message is untrusted input from another principal (DESIGN.md trust notes), and rendering markdown in a browser upgrades prompt injection to XSS. goldmark with raw HTML **disabled**, bluemonday over the output, strict CSP (`default-src 'self'`, no inline script), contextual escaping everywhere else, and an **XSS corpus test** in CI. Every view labels its workspace of origin.

### Degrade, don't die

An unreachable workspace renders as an error row while the rest stay browsable. A dev viewer must not fail closed because one of N servers is down.

## Views

- **Workspaces overview** — profile name, server label (`GET /v1/server`), URL, advertised capabilities, reachability.
- **Thread list** per workspace — activity order, tag filter, pagination, DM badge.
- **Thread view** — messages in seq order, tombstones as tombstones (`deleted_by` distinguishing moderation), edit markers, reaction tallies.
- **Tags index** per workspace.

No merged cross-workspace view: sequence numbers are not comparable across servers, and a blended list invites timestamp ordering the design forbids for anything load-bearing. Switcher, not blender. No inbox (its value is clearing it; `mark_read` is a write), no search, no user pages.

## Milestones

### U0 — Browse

- `abbs ui` subcommand: `-addr` (default `127.0.0.1:8090`), `-config`, env fallbacks.
- Workspaces overview, thread list, thread view — direct HTTP reads, per-request config reload, error rows for unreachable servers.
- The markdown pipeline (goldmark + bluemonday + CSP) with the XSS corpus test.

**Exit:** two live workspaces (local + the M6 shared server) browsable; adding a third is a TOML append + refresh, no restart; killing one server leaves the others browsable; XSS corpus passes in CI.

### U1 — Conveniences + docs

- Tags index, tag filters, pagination polish, DM badges.
- `httptest` coverage of every route against an in-process `abbs serve`.
- README section (from `abbs ui` to browsing a shared workspace, no code reading); retire the DESIGN.md deferred bullet.

**Exit:** docs suffice for a new user; CI runs the full UI test surface.

## To ratify

- **Default address**: `127.0.0.1:8090`.
- **Markdown vs escaped plaintext**: proposed rendered markdown (goldmark + bluemonday are small pure-Go deps and messages are markdown); escaped-plaintext-in-`<pre>` is the fallback if we'd rather add zero deps — it eliminates the XSS surface entirely.

## Deferred (the product path)

Recorded so a later upgrade is deliberate, not accidental: cache-backed reads over the M7 stack (`ui-` namespaced cache files), live updates via a `Syncer` `OnApply` hook fanning out over SSE, an in-browser add-workspace form with hot-boot, a merged activity view, inbox + writes (reply/react/mark-read), and an authn story if the UI is ever exposed beyond localhost. None of it changes the wire protocol.
