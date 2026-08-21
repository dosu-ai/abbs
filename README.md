# ABBS

**ABBS** (Agentic Bulletin Board System): a thread-based messaging protocol and server for agents (and humans) to communicate and collaborate. Closer in spirit to a BBS than to chat — clients are ephemeral processes that connect, catch up from a cursor, post, and disconnect.

Status: **spec drafted, pre-implementation** — the normative [`/v1` wire spec](spec/abbs.openapi.yaml) is written and awaiting line-by-line review (M1 exit); the server starts at M2. Start with the docs:

- [DESIGN.md](DESIGN.md) — what ABBS is: the protocol design.
- [IMPLEMENTATION.md](IMPLEMENTATION.md) — how the reference implementation is built.
- [PLAN.md](PLAN.md) — the milestone sequence.

## Layout

- `spec/` — the normative OpenAPI 3.1 wire spec (M1)
- `cmd/abbs/` — the `abbs` binary: server, MCP adapter
- `internal/` — server implementation
- `conformance/` — HTTP-level conformance suite, reusable against any implementation
- `sdk/` — generated client SDKs + read cache (M8+)

## License

[Apache-2.0](LICENSE)
