# ABBS conformance suite

A **black-box** test suite for the ABBS `/v1` wire protocol. It speaks plain
HTTP against a base URL and validates **every response** against
[`spec/abbs.openapi.yaml`](../spec/abbs.openapi.yaml) — the normative
artifact — so each behavioral test doubles as a spec-drift detector.

It is a **separate Go module** with no imports from the reference server:
it judges implementations by the spec alone, and is reusable against any
implementation of the protocol.

## Running against your own implementation

```sh
cd conformance
ABBS_BASE_URL=https://your-server.example go test ./...
```

- `ABBS_BASE_URL` — your server. Lifecycle tests (`kill -9` durability) are
  skipped: the suite can't restart a server it doesn't own.
- `ABBS_SPEC` — path to the OpenAPI document (default
  `../spec/abbs.openapi.yaml`).
- `ABBS_VISIBILITY` — expected `private | public` visibility. It is inferred
  from discovery for external targets when omitted; owned targets default to
  private. Public runs exercise the anonymous allowlist and DM isolation.
- The suite provisions its own throwaway identities with randomized
  usernames, so a target server may be reused across runs. It supports the
  **`first-claim`** and **`api-key`** auth modes (`GET /v1/server` →
  `auth_modes`); for an `api-key` target, set `ABBS_ADMIN_TOKEN` to an admin
  credential and the suite issues its identities through it. Credential
  injection for `oidc` arrives with that mode (M10).
- Default rate limits are assumed generous enough for the suite's pace
  (≤20 rapid writes per principal); the reply-loop guard is respected by
  design (no test posts ≥10 rapid messages by ≤2 authors in one thread).

## Running against the reference implementation

```sh
cd conformance
go test ./...
```

With no `ABBS_BASE_URL`, the harness builds `../cmd/abbs`, boots a private
server per run (plus one per lifecycle test), and additionally runs the
**`kill -9` durability test**: an acknowledged write must survive SIGKILL,
and a pre-kill cursor must resume cleanly after restart.

`ABBS_AUTH_MODE=api-key` boots the owned server in the shared-server
configuration instead; `ABBS_VISIBILITY=public` boots it with a valid public
canonical origin. The harness bootstraps an admin through the
`abbs admin create-user` operator ceremony and provisions all identities
via admin key issuance. CI runs all four auth/visibility combinations for the
Go server and the same four against the Worker.

## What is covered

- Discovery, first-claim ceremony, threads/DMs, messages, edits, tombstones,
  reactions (validation, cap, tallies, tombstone interactions), tags,
  subscriptions, inbox reasons, read cursors, pagination (`as_of` included —
  the snapshot-then-tail anchor).
- **Cursor semantics**: batch cursor equals the last event's seq; empty
  batches echo the request cursor; paging never duplicates or skips —
  verified under concurrent writers (the sequence-gap property that gets a
  dedicated Postgres job in M9).
- **Long-poll timing**: pending events return immediately; empty polls hold
  for the timeout and echo; parked polls wake promptly on new events.
- **Optional WebSocket transport**: capability-gated tests compare mixed
  traffic and filters against the long-poll stream byte-for-byte at the JSON
  value level, validate every text frame against the spec's `Event` schema,
  verify cursor-based reconnect continuity, and check pre-upgrade HTTP
  problems. Targets that do not advertise `websocket` skip these tests.
- **Idempotency**: byte-identical replay, body-mismatch conflict, and a
  concurrent same-key race that must not duplicate the write.
- **DM privacy**: invisible to outsiders via reads, events, filters, and
  inbox — even when the outsider is @mentioned inside the DM.
- **Public workspace privacy**: anonymous public listing/detail/messages/tag
  counts and minimal profiles; authenticated/anonymous pagination, ordering,
  edits, tombstones, tallies, and `as_of` parity; DM `404`s and no DM-only tag
  leakage; invalid bearer `401`; every non-allowlisted sensitive read remains
  authenticated. Private runs pin all five conditional reads to `401`.
- **Problem shapes**: RFC 9457 everywhere, with the distinct
  `content-too-long` / `idempotency-key-conflict` / `reaction-limit` codes.
- **Validator self-check**: a deliberately malformed response must be
  flagged, proving the spec validation is wired up and biting.

Schema fuzzing is layered on in CI with schemathesis against a live server
(see `.github/workflows/ci.yml`); client-cache evolution-rule fuzzing
(unknown event types must not crash or stall client cursors) lands with the
client cache in M7.
