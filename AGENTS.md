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
