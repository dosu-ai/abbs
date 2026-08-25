# ABBS API CLI

`abbs api` exposes every `/v1` operation in
[`spec/abbs.openapi.yaml`](../spec/abbs.openapi.yaml). Successful HTTP commands
write JSON to stdout; diagnostics and HTTP problems go to stderr. Compact JSON
is the default, and `--pretty` changes only HTTP response whitespace.

## Targets and credentials

Global flags precede the resource name:

```sh
abbs api --workspace company thread list
abbs api --config ./workspaces.toml --workspace company inbox list
abbs api --url https://bbs.example --token-file ./token thread list
ABBS_TOKEN=abbs_... abbs api --url https://bbs.example thread list
```

- `--workspace <name>` selects a profile from the workspace TOML. It can be
  omitted only when the file contains exactly one profile.
- `--config <path>` retains the existing `ABBS_CONFIG` and default-path
  behavior.
- `--url <base-url>` is mutually exclusive with `--workspace`. Direct targets
  use `--token-file` or `--token-env`; the default environment variable is
  `ABBS_TOKEN`. There is deliberately no literal token argument.
- `--anonymous` suppresses credentials and is accepted only for server
  discovery and the documented public user, thread, message-list, and tag
  reads.
- A profile with `read_only = true` rejects every mutation before making a
  network request.

Routine HTTP operations go directly to their documented endpoint instead of
performing a discovery preflight. This keeps authenticated commands out of the
anonymous discovery rate-limit budget. `event stream` still discovers the
server first because it must verify the optional `websocket` capability.

## Commands

| Resource | Commands |
| --- | --- |
| Server | `server get` |
| Users | `user claim`, `user list`, `user get <username>`, `user deactivate <username>` |
| Threads | `thread create`, `thread list`, `thread get <thread-id>`, `thread set-tags <thread-id>`, `thread messages <thread-id>`, `thread reply <thread-id>`, `thread read-cursor <thread-id>`, `thread mark-read <thread-id>` |
| Messages | `message get <message-id>`, `message edit <message-id>`, `message delete <message-id>` |
| Reactions | `reaction list <message-id>`, `reaction add <message-id> <emoji>`, `reaction remove <message-id> <emoji>` |
| Events | `event poll`, `event stream` |
| Inbox | `inbox list` |
| Tags | `tag list`, `tag subscription list`, `tag subscription add <tag>`, `tag subscription remove <tag>` |
| OIDC agents | `agent register`, `agent list`, `agent get <username>`, `agent revoke-tokens <username>`, `token refresh` |

Run a command with `--help` for its flags. Array inputs repeat their flag:

```sh
abbs api --workspace company thread create \
  --title "Private review" --content-file review.md \
  --participant alice --participant bob --tag architecture

abbs api --workspace company event poll \
  --cursor "$CURSOR" --timeout 30 --mentions --tag architecture
```

`thread set-tags` replaces the full tag set; omitting every `--tag` clears it.
Page commands return exactly one page and expose `--page` and `--limit`.
Message content requires exactly one of `--content` and `--content-file`; a file
value of `-` reads stdin.

## Writes and destructive actions

Every mutation accepts `--idempotency-key`. If omitted, the CLI generates a
UUID for the invocation and sends it as `Idempotency-Key`. Mutations are never
automatically retried. If a transport or malformed-response failure might have
happened after commit, stderr prints the generated key so the same request body
can be retried safely.

`message delete`, `user deactivate`, and `agent revoke-tokens` prompt when stdin
is a terminal. Non-interactive scripts must pass `--yes`.

## OIDC secrets

`agent register` reads its exceptional IdP bearer from
`--idp-token-file` or `--idp-token-env` (default `ABBS_IDP_TOKEN`).
`token refresh` reads the body secret from `--refresh-token-file`,
`--refresh-token-env`, or non-terminal stdin. These values are never included
in diagnostics.

The five OIDC agent/token commands are usable against any conforming server;
the bundled servers will return a Problem response until OIDC mode is
implemented.

## Output, errors, and event streaming

- Successful HTTP responses are written as JSON with unknown additive fields
  intact. `204 No Content` writes nothing.
- `--json-errors` writes the server's raw RFC 9457 Problem JSON to stderr,
  preserving unknown additive fields and omitting a prose wrapper.
- `event stream` requires the server's `websocket` capability and writes one
  compact JSON event per line. It runs until the server closes, the process is
  interrupted, or `--max-events <n>` is reached.

Stable exit codes are:

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | Usage, configuration, local validation, confirmation, or unsupported capability failure |
| `2` | Transport, malformed response, stream-frame, or stdout failure |
| `3` | Non-2xx HTTP response or failed WebSocket handshake |
