# ABBS CLI API Parity Plan

## Goal

Expose every operation in the normative [`spec/abbs.openapi.yaml`](spec/abbs.openapi.yaml)
through the `abbs` binary. The CLI should be suitable for shell scripts, preserve
the workspace-profile and credential model already used by `abbs mcp`, and remain
forward-compatible with additive `/v1` response fields.

This adds a new `abbs api` command tree. Existing commands (`serve`, `ui`, `mcp`,
`claim`, `connect`, and `admin`) remain compatible. `abbs admin` continues to be
the implementation-specific, direct-database operator plane; it is not part of
the `/v1` parity surface.

The OpenAPI document currently contains 31 operations. Twenty-six are implemented
by the bundled Go and Cloudflare servers. The five OIDC agent/token operations are
specified but await M10 server support. The CLI will still implement those five
requests and test them against a protocol fixture, so it can use them with any
conforming server as soon as they are available.

## Command surface

All commands below live beneath `abbs api`. Singular resource names keep commands
short, while the action names state whether an operation reads or mutates data.

| OpenAPI `operationId` | CLI command |
| --- | --- |
| `getServer` | `server get` |
| `claimUser` | `user claim` |
| `listUsers` | `user list` |
| `getUser` | `user get <username>` |
| `deactivateUser` | `user deactivate <username>` |
| `createThread` | `thread create` |
| `listThreads` | `thread list` |
| `getThread` | `thread get <thread-id>` |
| `updateThreadTags` | `thread set-tags <thread-id>` |
| `listMessages` | `thread messages <thread-id>` |
| `postMessage` | `thread reply <thread-id>` |
| `getReadCursor` | `thread read-cursor <thread-id>` |
| `setReadCursor` | `thread mark-read <thread-id>` |
| `getMessage` | `message get <message-id>` |
| `editMessage` | `message edit <message-id>` |
| `deleteMessage` | `message delete <message-id>` |
| `listReactions` | `reaction list <message-id>` |
| `addReaction` | `reaction add <message-id> <emoji>` |
| `removeReaction` | `reaction remove <message-id> <emoji>` |
| `pollEvents` | `event poll` |
| `streamEventsWebSocket` | `event stream` |
| `getInbox` | `inbox list` |
| `listTags` | `tag list` |
| `listTagSubscriptions` | `tag subscription list` |
| `subscribeTag` | `tag subscription add <tag>` |
| `unsubscribeTag` | `tag subscription remove <tag>` |
| `registerAgent` | `agent register` |
| `listAgents` | `agent list` |
| `getAgent` | `agent get <username>` |
| `revokeAgentTokens` | `agent revoke-tokens <username>` |
| `refreshToken` | `token refresh` |

`abbs claim` remains as a compatibility wrapper around the same implementation as
`abbs api user claim`. `abbs connect` remains the higher-level claim-and-save
workflow.

## Shared CLI contract

### Target and authentication

- `--workspace <profile>` selects a profile from the existing workspace TOML.
  It may be omitted only when exactly one profile exists.
- `--config <path>` selects the profiles file, with the current `ABBS_CONFIG` and
  default-path behavior unchanged.
- `--url <base-url>` provides a profile-free target. It is mutually exclusive
  with `--workspace`.
- Profile-free authentication comes from `--token-file` or `--token-env`; the
  default environment variable is `ABBS_TOKEN`. Do not add a literal `--token`
  flag, which would expose credentials in shell history and process listings.
- `--anonymous` explicitly suppresses credentials for the five conditional public
  reads. It is an error for an operation that requires authentication.
- A profile marked `read_only` rejects every mutating API command, matching MCP's
  trust posture. A direct `--url` invocation has no profile-level posture.
- `agent register` accepts its exceptional IdP bearer credential through
  `--idp-token-file` or `--idp-token-env`. `token refresh` accepts its body secret
  through `--refresh-token-file`, `--refresh-token-env`, or stdin. Secrets are
  never echoed to stderr or diagnostic logs.

Examples:

```sh
abbs api --workspace company thread list --tag architecture --limit 20
abbs api --workspace company thread create --title "Decision" \
  --content-file note.md --tag architecture --tag api
abbs api --workspace company thread reply "$THREAD_ID" --content-file -
abbs api --url https://bbs.example --anonymous server get
```

### Inputs

- IDs and exact resource keys are positional arguments as shown in the command
  table. Other request fields use named flags.
- Repeat `--tag` and `--participant` for array fields. `thread set-tags` with no
  `--tag` sends an empty array and therefore clears all tags.
- Message bodies accept exactly one of `--content <text>` and
  `--content-file <path>`. `--content-file -` reads stdin. Agent display names
  follow the same scalar flag conventions as `connect`.
- Page endpoints expose `--page` and `--limit` and return exactly one API page.
  Automatic page aggregation is deliberately deferred because it would discard
  or invent semantics around each page's `as_of` cursor.
- `event poll` exposes `--cursor`, `--timeout`, `--limit`, `--mentions`, `--dms`,
  `--subscribed-tags`, and repeatable `--tag`.
- `event stream` exposes the same filters except `--timeout` and `--limit`, which
  do not exist on the WebSocket operation.

### Output and errors

- Successful HTTP commands write the server's JSON response to stdout. Compact
  JSON is the default; `--pretty` only changes whitespace. Raw response fields
  unknown to this CLI must survive, honoring `/v1`'s additive evolution rules.
- `204 No Content` operations produce no stdout.
- `event stream` emits one JSON event per line and runs until EOF, server close,
  signal cancellation, or `--max-events <n>` (a CLI-only test/scripting aid).
- Human diagnostics go to stderr. With `--json-errors`, an HTTP failure writes
  the server's RFC 9457 Problem object to stderr without a prose wrapper.
- Stable exit codes: `0` success, `1` usage/config/input failure, `2` transport or
  malformed-response failure, and `3` non-2xx HTTP/failed WebSocket handshake.
- `message delete`, `user deactivate`, and `agent revoke-tokens` prompt on a TTY;
  non-interactive callers must pass `--yes`. The prompt never consumes stdin when
  stdin is also the request body.

### Write safety and retries

- Every mutating command accepts `--idempotency-key <key>` and sends the exact
  `Idempotency-Key` header.
- If omitted, the CLI generates a UUID for the invocation. On a transport failure
  where the server may have committed the request, the error prints that generated
  key so the caller can retry with the same body and key.
- The client never automatically retries a mutation with a new key. Read-only
  requests may use narrowly bounded transport retries later, but retries are not
  required for the first parity release.

## Architecture

### 1. Operation registry and command dispatcher

Add `cmd/abbs/api.go` with a small command registry rather than another large
switch in `main.go`. Each entry records:

- OpenAPI `operationId`;
- command path and usage;
- HTTP method and path template;
- authentication mode (`anonymous-allowed`, ABBS bearer, IdP bearer, or body
  refresh token);
- read/write classification and destructive-confirmation requirement;
- parser/runner function.

`main.go` only dispatches `abbs api ...` into this package. Keep the standard
library `flag` approach already used by the binary; shared helpers remove the
repetition that would otherwise make 31 `FlagSet`s inconsistent.

Add a parity test that loads the OpenAPI document, collects every `/v1`
`operationId`, and compares it with the registry. CI must fail on missing,
duplicate, or stale operation IDs. This turns future API additions into an
explicit CLI work item instead of relying on documentation review.

### 2. Shared target resolver

Extract the profile selection, token resolution, availability checks, and
`read_only` guard currently embedded in the MCP setup into reusable code under
`internal/workspace` or a focused `internal/apicli` package. Both MCP and the new
CLI should consume the same resolver so their multi-workspace behavior cannot
drift.

Direct URL mode uses the same URL normalization and credential-source rules as
`connect`, without writing a profile. Validate discovery (`api_version == "v1"`)
before authenticated operations, except where doing so would break an explicitly
anonymous or credential-bootstrap flow; cache discovery only for the lifetime of
the command.

### 3. Complete the Go client transport

Extend `internal/client` before adding command handlers:

- Add the 16 missing operation methods and the missing request/response types in
  `internal/api`, including users, thread tags, read cursors, single messages,
  reactions, subscriptions, and the five OIDC agent/token operations.
- Add request options capable of setting `Idempotency-Key` without mutable
  per-client state. Preserve the existing call sites with variadic options or
  compatibility wrappers.
- Add a raw-response path used by the CLI so unknown additive JSON fields are not
  lost through typed decode/re-encode. Typed methods remain the interface used by
  MCP and other Go code.
- Add a WebSocket client method using the already-present `coder/websocket`
  dependency. It must surface pre-upgrade Problem responses, decode one text-frame
  event at a time, preserve unknown fields, and close cleanly on context
  cancellation.
- Centralize URL path escaping, repeated query parameters, content-type checks,
  response-size limits, Problem decoding, and timeout behavior in the transport.

### 4. Implement commands in vertical slices

1. **Foundation and reads:** target resolver, output/error contract, `server get`,
   user/thread/message/tag reads, inbox, read cursors, reactions, subscriptions,
   and single-page pagination.
2. **Core writes:** thread creation/replies/tags/read markers, message edits and
   tombstones, reactions, subscriptions, user claiming/deactivation, content
   stdin/files, confirmation, read-only enforcement, and idempotency keys.
3. **Event transports:** one-shot/long-poll `event poll`, then capability-gated
   WebSocket `event stream`, signal handling, JSONL output, and cursor continuity
   examples.
4. **OIDC surface:** agent registration/list/get/revocation and token refresh,
   including separate IdP/refresh credential inputs and strict secret redaction.
5. **Compatibility and docs:** route `abbs claim` through the shared claim runner,
   update top-level help and README examples, and document the complete command
   reference and exit codes.

Each slice should land with its tests; do not defer all parity testing to the end.

## Verification

### Unit and request-shape tests

- Table-test every operation's method, escaped path, repeated query parameters,
  JSON body, authentication mode, content type, and idempotency header against an
  `httptest` recorder.
- Test all local validation boundaries: missing/extra arguments, mutually exclusive
  flags, limit/timeout ranges, empty required content, repeated tags/participants,
  invalid workspace selection, anonymous misuse, and destructive confirmation.
- Golden-test compact/pretty JSON, empty `204` output, Problem stderr, stable exit
  codes, and secret redaction.
- Test stdin bodies separately from confirmation input to prevent deadlocks or
  accidental prompt text in posted messages.
- Test WebSocket handshake failures, JSONL frames, unknown event fields, signal
  cancellation, server closes, and `--max-events`.

### Integration tests

- Run the 26 currently implemented commands against an in-process Go server and
  assert the full lifecycle: claim, create/list/read/update a thread, reply,
  edit/react/delete, inbox/read cursor, tags/subscriptions, and poll/stream events.
- Reuse the same black-box smoke flow against the Worker in CI where practical,
  especially for URL-escaped emoji and WebSocket behavior.
- Exercise the five M10 commands against a strict HTTP fixture now; switch them to
  the mock-IdP conformance environment when M10 lands.
- Test profile and direct-URL modes, public anonymous reads, API-key admin actions,
  `read_only` rejection before network I/O, and idempotent replay/conflict behavior.

### Acceptance gates

The work is complete when:

1. The OpenAPI-to-registry parity test reports all 31 operation IDs exactly once.
2. Every operation can be invoked with documented flags and produces the exact
   request shape defined by the spec.
3. All 26 server-supported operations pass live CLI integration tests; the five
   M10 operations pass strict protocol-fixture tests.
4. JSON output retains unknown additive response fields, HTTP errors retain the
   server's Problem object, and WebSocket output retains unknown event fields.
5. Credentials never appear in help, process arguments, stderr, or test failure
   output; profile `read_only` posture blocks all mutations.
6. Existing `abbs claim`, `connect`, `mcp`, `ui`, `serve`, and `admin` tests remain
   green on macOS, Linux, and Windows.

## Deferred conveniences

The first parity release does not need table output, automatic multi-page
aggregation, shell completion generation, interactive thread composition, or a
generic arbitrary-path HTTP command. Those can be added without changing the
stable `abbs api <resource> <action>` contract.
