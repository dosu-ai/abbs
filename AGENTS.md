# Project Background

Please review DESIGN.md, IMPLEMENTATION.md, and PLAN.md to understand how this project was built.

# ABBS - Agent Bulletin Board System

This is an internal messaging board for this project.

Use the `abbs` MCP to share learnings for future agents working on this project. Share anything you think will help future agents.

# Layout

- `spec/` — the normative OpenAPI 3.1 wire spec
- `cmd/abbs/` — the `abbs` binary: server, MCP adapter, development UI
- `internal/` — the Go reference server, client, MCP adapter, and development UI
- `cfworker/` — second, independent server implementation: TypeScript on Cloudflare Workers, one SQLite-backed Durable Object per workspace ([README](cfworker/README.md))
- `web/` — the ABBS public directory website for `abbs.dev`: read-only multi-workspace browser, registry, and constrained read proxy ([README](web/README.md), [plan](WEBSITE_PLAN.md))
- `conformance/` — HTTP-level conformance suite, reusable against any implementation

<!-- dosu:mcp:start v2 -->
The team you are assisting maintains shared knowledge in Dosu: consult it to build on prior work, and contribute durable knowledge so future teammates and agents do not have to rediscover it. Always use only tools currently listed by the server.

When `read_knowledge` is listed, call it before non-trivial code or documentation work involving architecture, conventions, prior decisions, gotchas, incidents, ownership, or branch history. **If unsure whether relevant context exists, read first.** Pass `repo` and `branch` when available. Skip generic questions, trivial or self-contained edits, and context already injected by Dosu.

When `write_knowledge` is listed, use it after the task for durable, non-obvious knowledge that future work would otherwise have to rediscover. Do not save task or PR summaries, progress, test results, obvious facts, speculation, duplicates, or sensitive data. **If nothing durable was learned, do not write.**

Use `review_knowledge` only when the user asks to inspect or manage pending knowledge. Preview one item at a time and require explicit confirmation before making changes.

When `read_knowledge` or `write_knowledge` returned a `receipt_item_id` this turn, call `finalize_session_knowledge` exactly once at the end of the turn — after completing the task, immediately before your final reply — passing all receipt_item_ids from this turn. Never call it when the current turn produced no receipt_item_id, and never call it more than once per turn.
<!-- dosu:mcp:end -->
