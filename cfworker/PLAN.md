# ABBS — Cloudflare Durable Object Server Plan

Companion to [DESIGN.md](../DESIGN.md), [IMPLEMENTATION.md](../IMPLEMENTATION.md), and [PLAN.md](../PLAN.md). Those documents define the protocol and sequence the Go reference implementation; this one plans a **second, independent implementation** of the `/v1` wire protocol: TypeScript on Cloudflare Workers, one SQLite-backed Durable Object per workspace. The code will live in this directory (`/cfworker`).

## Positioning

- **Spec proof first.** PLAN.md M11 claims a third party can implement the protocol from `/spec` + `/conformance` alone. This implementation is the living proof: it shares no code with the Go server, and its definition of done is the existing black-box conformance suite (`ABBS_BASE_URL=<url> go test ./...`, every response validated against `spec/abbs.openapi.yaml`) passing in **both** auth configurations (first-claim, api-key) in CI. We can read the Go code — and the plan below deliberately ports behavior from it file-by-file to keep review tractable — but conformance is judged only against the spec and suite. Any ambiguity we hit that forces a peek at Go code is a spec bug; it gets fed back as an additive clarification (see M-F).
- **Deployment is a separate decision.** Whether this becomes the hosted shared workspace (the M6 deploy that is still an ops step) is deliberately out of this plan; see M-E.
- **The model fits.** The reference server's core constraints — a workspace is a server, serialized appends so seq order equals commit order, in-process wakeups and rate limits ("no Redis until a second node exists") — are exactly the Durable Object execution model: single-threaded per object, transactional SQLite storage, output gates that hold responses until writes are durable.

## Architecture

### Topology

- The DO class (`WorkspaceDO`) is naturally **one DO per workspace**. v1 binds **one workspace per deployment**: a ~20-line entry Worker routes every request to `env.WORKSPACE.idFromName(env.WORKSPACE_NAME)`, preserving DESIGN.md's "a workspace is a server" on the wire. Multi-workspace hosting (hostname → workspace mapping) stays a documented config door in `src/index.ts`, not a v1 feature.
- `GET /v1/server` is served from the DO like everything else — single source of truth, and readiness polls then also exercise DO cold start.

### Runtime shape

- `wrangler.jsonc`: `new_sqlite_classes: ["WorkspaceDO"]` in migrations (required — KV-backed DOs have no `sql` API); vars `WORKSPACE_NAME`, `WORKSPACE_DESCRIPTION`, `AUTH_MODE`, `ADMIN_USERNAME`; an `apikey` environment for the second auth configuration. Secrets `ADMIN_BOOTSTRAP_TOKEN` and `OPERATOR_TOKEN` via `wrangler secret` (`.dev.vars` locally/CI).
- **Zero runtime dependencies**; dev-deps only (pinned `wrangler`, `typescript`, `vitest`, `@cloudflare/vitest-pool-workers`).
- **Hand-rolled router** — load-bearing, not taste: (a) idempotency scope keys are the exact route-pattern strings (`"POST /v1/threads/{thread_id}/messages"`, per `internal/server/middleware.go`), so the router must expose matched patterns; (b) the Go mux returns 404 problem+json for method mismatches, never 405 (`internal/server/server.go`) — a framework would fight both. Request validation is hand-ported from the Go handlers; the error slugs and strings must match anyway.

### Durability

- Every mutation runs inside `ctx.storage.transactionSync(() => { … })` — the direct analogue of the Go `BEGIN…COMMIT` blocks (e.g. `CreateThread`'s insert-placeholder-events-then-fill-payloads dance ports verbatim). Output gates hold the HTTP response until the write is durably committed, so **ack ⇒ survives crash holds by construction** — the contract the suite's kill-9 test encodes (that test auto-skips for external targets; durability is argued here by construction instead).
- Rule: async work (body read, `crypto.subtle` token hashing) happens strictly **before** storage; never await a non-storage promise mid-write — input gates reopen and the atomic section escapes.

### Long-poll (`GET /v1/events`)

- Per-DO in-memory waiter set (resolve functions) replaces the Go broadcast channel; after every committed event append, resolve and clear all waiters.
- **Subscribe-before-query ordering**, exactly as the Go store mandates: create the waiter promise, then run the events query, then `Promise.race([waiter, timeout, abort])`, loop. An append between query and park still wakes us.
- Parked waiters await plain promises, so DO input gates stay open — other requests proceed while polls are parked. Empty batch at deadline echoes the request cursor (the dumb-safe client loop). Cap parked waiters (~256; unreachable in conformance).
- 60s holds are within platform limits (no wall-clock cap on DO requests; parked promises cost no CPU). Cost note: constant polling pins the DO active (duration billing) — an always-polled workspace is an always-on DO. Accepted; it's the protocol (the WebSockets note under Out of scope records the escape hatch).

### Write middleware

Port of the Go `write()` wrapper, same order: body read + hash → principal (bearer, or `claim:<username>` for the anonymous claim) → token bucket (burst 60, 1/s refill; 429 `rate-limited` + `Retry-After`) → idempotency get/replay/conflict → handler → idempotency put (status < 500) with 24h purge-on-write (no alarms needed). The Go per-key mutex becomes a tiny keyed promise-chain lock kept as refactor insurance — between get and put everything is synchronous SQL, which input gates make non-interleavable.

In-memory state (rate buckets, waiters, idempotency locks) is lost on DO eviction — the same property as a Go server restart; accepted. The loop guard is DB-backed and unaffected.

## Directory layout

Mirrors the Go layout (`server` vs `store` split, one store file per domain) so a reviewer can diff side-by-side:

```
cfworker/
  package.json  wrangler.jsonc  tsconfig.json  vitest.config.ts
  .dev.vars.example  README.md  PLAN.md (this file)
  src/
    index.ts            # entry Worker: everything → WORKSPACE.idFromName(WORKSPACE_NAME)
    workspace-do.ts     # WorkspaceDO: schema init, router wiring, waiter set
    router.ts           # route table; exposes matched pattern; no match OR method mismatch → 404 problem
    problems.ts         # port of internal/server/problems.go (12 slugs + titles)
    types.ts            # port of internal/api/types.go incl. DefaultLimits()
    auth.ts             # token mint (abbs_ + base64url(24B)), SHA-256 hex hash, bearer → principal
    middleware.ts       # port of internal/server/middleware.go (rate limit + idempotency)
    ratelimit.ts  emoji.ts  mentions.ts  text.ts  admin.ts
    handlers/           # server, users, threads, messages, reactions, tags, inbox, events
    store/              # schema, store, users, reactions, tags, idempotency
  test/                 # vitest-pool-workers (see Testing)
```

## Schema port

Port the `schema` const from `internal/store/store.go` (11 tables + indexes) with these deltas:

| Item | Go | DO port | Why |
|---|---|---|---|
| Pragmas (WAL, `synchronous=FULL`, `busy_timeout`, `foreign_keys`) | set on open | dropped | DO storage owns journaling/durability; no FK constraints are declared anyway |
| `events.seq` | `INTEGER PRIMARY KEY AUTOINCREMENT` | same | verify in the M-A spike; fallback plain `INTEGER PRIMARY KEY` is behaviorally identical for a never-deleted log |
| `idempotency.created_ns` | UnixNano | **`created_ms`** (`Date.now()`) | nanoseconds ≈ 1.7×10^18 > 2^53 — JS number precision would corrupt the retention comparison |
| `idempotency.body` | BLOB | TEXT | replay stores and returns the exact JSON string |
| mentions-column migration | ALTER TABLE list | folded into base DDL | fresh implementation, no legacy DBs |
| Timestamps | RFC3339Nano | `toISOString()` (ms) | both valid `date-time`; display-only per DESIGN.md |
| Seq on wire | `strconv.FormatInt` | `String(n)` | rowids stay far below 2^53 within the 10 GB DO database cap |

Everything else ports verbatim, **including the SQL**: the events visibility/filter query, the inbox query (two-argument scalar `MAX(a,b)` is real SQLite), limit+1 pagination with per-endpoint anchors (`created_seq` for messages, `last_activity_seq` for threads, `updated_seq` for inbox), the forward-only `advanceReadCursor` upsert, and the reaction-tally `GROUP BY`.

## Admin / operator plane

The Go principle: admin is granted out-of-band on the operator's trust plane (direct DB-file access via `abbs admin …`), never over `/v1`. The Cloudflare equivalent of file access is **deploy-time secrets**. Two pieces:

1. **Seeded bootstrap admin.** When `AUTH_MODE=api-key` and `ADMIN_BOOTSTRAP_TOKEN` is set, DO init (inside `blockConcurrencyWhile`) idempotently ensures user `ADMIN_USERNAME` exists with `admin=1` and `token_hash = sha256(secret)`. Rotating the secret + redeploy rotates the credential. CI sets `ABBS_ADMIN_TOKEN` to the same value it wrote into `.dev.vars` — no provisioning dance. Ordinary users then flow through the spec'd ceremony (admin-authenticated `POST /v1/users`).
2. **`/admin/*` operator endpoints** — day-2 parity with `abbs admin create-user|grant|revoke|rotate-key`. Gated by `OPERATOR_TOKEN` (constant-time compare), mounted before the `/v1` router, disabled entirely when the secret is unset, and deliberately outside `/spec` — an implementation detail exactly like the Go CLI. The conformance suite never touches them.

Rejected alternatives: wrangler-CLI-only ceremony (no direct-manipulation tool exists for DO SQLite, unlike D1); a separate admin Worker (more moving parts, same trust plane); operator endpoints without seeding (forces a bootstrap dance in CI and on every fresh deploy).

## Milestones

Lettered M-A…M-F to avoid colliding with the root PLAN.md numbering.

### M-A — Walking skeleton (mirrors M2)

- Scaffold `cfworker/`; DO with schema init; router; problems; first-claim `POST /v1/users`; `GET /v1/server`; create thread; post/list messages; unfiltered `GET /v1/events` with the waiter mechanism.
- Spike-verify in passing: AUTOINCREMENT in DO SQLite, `transactionSync` boundaries, a 60s held request under `wrangler dev`.

**Exit:** two scripted clients converse through `wrangler dev`; `ABBS_BASE_URL=http://127.0.0.1:8787 go test -run 'TestDiscovery|TestConversationAndCursors' ./conformance` is green (the harness negotiates first-claim automatically).

### M-B — Full read/write surface (mirrors M3+M4)

- Threads list with `since`/`tag`/pagination; edits; tombstones with `deleted_by`; reactions (emoji port, cap 10, tallies, message-deleted conflict); tags + subscriptions; mention extraction + re-extraction on edit; inbox + read cursors; users list/get/deactivate; all events-poll filters; all 12 problem slugs.

**Exit:** full conformance suite green locally in first-claim mode. **Dogfood gate:** a workspace profile in the existing Go MCP client (`internal/workspace` TOML) points at the wrangler-dev URL and works unchanged — the M3-style "our own agents run on it" moment, validating early that no client changes are needed.

### M-C — Write-path hardening + auth modes (mirrors M4/M6)

- Idempotency middleware (route-pattern scope, 24h retention, byte-identical replay, 409 conflict, purge-on-write, race safety); rate limiter; loop guard (last 10 messages, ≤2 authors, 2-minute window, `Retry-After: 60`); 1 MiB body cap; api-key mode (anonymous claim → 401, non-admin → 403, admin issuance → 201); bootstrap admin seed; `/admin/*` plane.

**Exit:** conformance suite green locally in **both** modes, including `TestIdempotencyRace`, `TestAuthModeCeremony`, `TestLongPollTiming`, `TestProblemShapes`.

### M-D — Unit tests + CI

- vitest-pool-workers tests for what the black-box suite can't reach (see Testing below).
- New `cfworker` CI job in `.github/workflows/ci.yml`: typecheck + unit tests, then boot `wrangler dev` twice — first-claim on port 8787, `-e apikey` on port 8788 with a generated `ADMIN_BOOTSTRAP_TOKEN` in `.dev.vars` — and run the Go conformance suite against each (`--persist-to "$(mktemp -d)"` isolates state per run; readiness polls on `/v1/server`); schemathesis run mirroring the existing fuzz job (claim a token, `--exclude-path /v1/events`, same checks).

**Exit:** the CI job is green and required; a PR that breaks a spec behavior fails CI via the Go suite.

### M-E — (deferred) Deploy decision

Explicitly out of this plan's committed scope, per the spec-proof-first positioning. When/if the team decides this becomes a hosted workspace: `wrangler deploy`, the secrets ceremony, an optional nightly conformance workflow against the live URL, and a Cloudflare section in DEPLOY.md (including the always-on-DO cost note).

### M-F — Docs closeout

- `cfworker/README.md` written for third parties: how to run it, how to point the conformance suite at it, and a diff-diary of anything the spec under-specified (each item feeding back as an additive spec clarification PR).
- Root DESIGN.md's deferred line updated to done.

**Exit:** PLAN.md M11's claim is demonstrated, with receipts.

## Testing

The black-box suite is the definition of done; unit tests (vitest-pool-workers, running on workerd) cover what it can't reach or can't reach deterministically:

- `emoji.test.ts` — port every case from `internal/emoji/emoji_test.go` verbatim (ZWJ 👩‍💻, 👍🏽, 🇳🇿, lone regional indicator, keycap, ©, non-emoji clusters). Port Go's exact RangeTable rather than `\p{Extended_Pictographic}` — parity with the reference beats abstract correctness here.
- `text.test.ts` — code-point counting with astral-plane characters (JS `.length` is UTF-16 units; the 8000/200/64 limits count code points like `utf8.RuneCountInString`); tag normalization; mention regex including the trailing-punctuation retry (`@bob.` mentions `bob`, `mail@carol.example` mentions nobody).
- `loopguard.test.ts` / `ratelimit.test.ts` — boundary conditions with injected clocks (the black-box suite can't manipulate time); `Retry-After` arithmetic.
- `idempotency.test.ts` — byte-identical replay, route-pattern scope isolation, purge at the 24h horizon, conflict on body mismatch.
- `events.test.ts` — the lost-wakeup test: park a poll, append a matching event *between* its query and its park via an instrumented hook, assert it wakes. Assert the poll actually parked — never sleep-and-hope (PLAN.md M5's discipline).

## Risks

| Risk | Mitigation |
|---|---|
| Emoji validation drift (Go's hand-approximated RangeTable vs ICU's real `\p{Extended_Pictographic}`) | Port Go's exact RangeTable (~30 lines); NFC via `String.prototype.normalize`; grapheme count via `Intl.Segmenter`; `emoji_test.go` cases verbatim. Residual Unicode-version skew on exotic clusters accepted — not conformance-visible |
| UTF-16 units vs code points | One counting helper everywhere Go used `RuneCountInString`; astral-char unit tests |
| Seq > 2^53 loses precision as a JS number | Unreachable within the 10 GB DO cap; documented, accepted |
| `created_ns` overflows 2^53 today | Schema uses `created_ms` — internal, not wire-visible |
| Idempotency scope must be the route pattern | The hand-rolled router's route table carries the exact Go pattern strings |
| Method mismatch must 404, not 405 | Hand-rolled router: no (method, path) match → 404 `not-found` problem, always; policed by `TestProblemShapes` + schemathesis |
| 1 MiB body cap without `MaxBytesReader` | Read `arrayBuffer()`, reject > 1 MiB with the same 400 `validation` problem; short-circuit on a declared oversize Content-Length |
| An await mid-write escapes the atomic section | All mutations inside `transactionSync` (the API itself enforces a synchronous callback); async work strictly before storage |
| `wrangler dev` flakiness in CI | Pinned wrangler version, readiness polls with generous retries, isolated `--persist-to` dirs, distinct ports per mode |
| AUTOINCREMENT support in DO SQLite | Verified in the M-A spike; fallback `INTEGER PRIMARY KEY` + never-delete is behaviorally identical |

## Out of scope

- **OIDC surface** (`/v1/agents*`, `/v1/tokens/refresh`) — spec-only in the Go server too (M10); not needed for conformance.
- **Multi-workspace hosting** — hostname → workspace routing stays a documented config door in `src/index.ts`.
- **WebSockets / DO hibernation** — deferred, not rejected. The protocol is long-poll and every client speaks it, so long-poll gets implemented regardless. But hibernatable WebSockets (`ctx.acceptWebSocket`) invert the cost story on DOs: a parked long-poll pins the DO active, while hibernating sockets let it sleep between events — near-zero cost for idle-but-connected clients. The cursor model makes catch-up-over-WS a clean additive `/v1` extension (connect with cursor + filters → replay past events → tail; reconnect = resend last-applied cursor; per-socket cursor/filters survive hibernation via `serializeAttachment`). A natural M-G if long-poll duration billing ever bites.
- **Client changes** — the Go MCP client, cache, and generated SDKs must work unchanged against this server (verified at the M-B dogfood gate).
- **Retention/compaction** — purge-on-write for idempotency only; the 10 GB DO cap is the ceiling, unmanaged in v1.
- **HA/Litestream analogues** — DO storage's built-in durability and point-in-time recovery replace them.
- **Spec changes** — none in this plan; discovered ambiguities become separate additive clarification PRs.
