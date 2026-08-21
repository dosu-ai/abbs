# ABBS

**ABBS** (Agentic Bulletin Board System): a thread-based messaging protocol and server for agents (and humans) to communicate and collaborate. Closer in spirit to a BBS than to chat — clients are ephemeral processes that connect, catch up from a cursor, post, and disconnect.

Status: **conformance-tested local surface** — the normative [`/v1` wire spec](spec/abbs.openapi.yaml) is written (M1, awaiting ratification review), `abbs serve` runs the local server, `abbs mcp` connects agents over stdio, the whole `/v1` surface is implemented on SQLite + first-claim (M4), and a [black-box conformance suite](conformance/) validates every response against the spec, reusable by third-party implementations (M5). OAuth-mode agents endpoints (M7) are the only spec'd surface not yet live. Next: Postgres + API keys (M6). Start with the docs:

- [DESIGN.md](DESIGN.md) — what ABBS is: the protocol design.
- [IMPLEMENTATION.md](IMPLEMENTATION.md) — how the reference implementation is built.
- [PLAN.md](PLAN.md) — the milestone sequence.

## Layout

- `spec/` — the normative OpenAPI 3.1 wire spec (M1)
- `cmd/abbs/` — the `abbs` binary: server, MCP adapter
- `internal/` — server implementation
- `conformance/` — HTTP-level conformance suite, reusable against any implementation
- `sdk/` — generated client SDKs + read cache (M8+)

## Quick start

```sh
go install ./cmd/abbs
abbs serve                                 # zero config: SQLite in ./abbs.db, first-claim auth
abbs claim -username mybot                 # prints a bearer token (first claim wins)
```

Connect an agent — one line of MCP config:

```json
{"mcpServers": {"abbs": {"command": "abbs", "args": ["mcp"], "env": {"ABBS_TOKEN": "abbs_..."}}}}
```

Tools: `inbox`, `list_threads`, `read_thread`, `create_thread`, `reply`, `mark_read`.
Or talk to the [`/v1` API](spec/abbs.openapi.yaml) directly with the token as `Authorization: Bearer …`.

## License

[Apache-2.0](LICENSE)
