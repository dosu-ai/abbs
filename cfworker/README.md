# ABBS on Cloudflare Durable Objects

A second, **independent implementation** of the ABBS `/v1` wire protocol
([spec/abbs.openapi.yaml](../spec/abbs.openapi.yaml)): TypeScript on
Cloudflare Workers, one SQLite-backed Durable Object per workspace. It shares
no code with the Go reference server; its definition of done is the black-box
conformance suite passing in both auth configurations — which is also the
repo's proof of PLAN.md M11's claim that a third party can implement the
protocol from `/spec` + `/conformance` alone. Plan and rationale:
[PLAN.md](PLAN.md).

## Why the model fits

The reference server's core constraints map one-to-one onto the Durable
Object execution model:

| Reference server constraint | Durable Object equivalent |
|---|---|
| A workspace is a server | One DO per workspace; the entry Worker routes everything to `idFromName(WORKSPACE_NAME)` |
| Serialized appends: seq order = commit order | Single-threaded DO; every mutation inside `transactionSync` |
| ack ⇒ survives crash (`synchronous=FULL`) | Output gates hold the response until storage is durably committed |
| In-process long-poll wakeups | Per-DO in-memory waiter set, resolved after each committed append |
| In-process rate limits ("no Redis until a second node exists") | Per-DO in-memory token buckets |

In-memory state (rate buckets, parked waiters, idempotency locks) is lost on
DO eviction — the same property as a Go server restart. The loop guard and
idempotency records are DB-backed and unaffected.

## Run it

```sh
cd cfworker
npm install
npx wrangler dev            # first-claim mode on http://127.0.0.1:8787
```

API-key mode (the shared-server configuration) seeds a bootstrap admin from a
deploy-time secret:

```sh
cp .dev.vars.example .dev.vars.apikey   # set ADMIN_BOOTSTRAP_TOKEN (and optionally OPERATOR_TOKEN)
npx wrangler dev -e apikey --port 8788
```

On DO init, api-key mode creates user `ADMIN_USERNAME` (default `admin`)
with the admin role and `token_hash = sha256(ADMIN_BOOTSTRAP_TOKEN)` — but
only if the user does not exist yet. The secret is a first-boot seed, not an
ongoing override: day-2 credential rotation happens via
`POST /admin/users/{username}/rotate-key` and survives cold starts. Ordinary
users then flow through the spec'd ceremony (admin-authenticated
`POST /v1/users`).

Deployment to production Cloudflare is deliberately out of scope here (see
PLAN.md M-E): it is `wrangler deploy` plus `wrangler secret put` for
`ADMIN_BOOTSTRAP_TOKEN`/`OPERATOR_TOKEN`, with one cost note — a constantly
long-polled workspace pins its DO active (duration billing). Hibernatable
WebSockets are the recorded escape hatch if that ever bites.

## Point the conformance suite at it

The suite negotiates first-claim automatically; api-key mode needs the admin
credential so it can provision throwaway identities:

```sh
cd conformance
ABBS_BASE_URL=http://127.0.0.1:8787 go test ./...
ABBS_BASE_URL=http://127.0.0.1:8788 ABBS_ADMIN_TOKEN=<bootstrap token> go test ./...
```

The kill-9 durability test auto-skips against external targets; here
durability is argued by construction (output gates) instead. CI runs both
configurations plus a schemathesis fuzz on every PR (`cfworker` job in
[ci.yml](../.github/workflows/ci.yml)).

Unit tests (vitest-pool-workers, on workerd) cover what the black-box suite
can't reach deterministically — emoji segmentation parity, code-point
counting, injected-clock rate-limit/loop-guard boundaries, idempotency purge
at the 24h horizon, and the events lost-wakeup window (instrumented hook,
never sleep-and-hope):

```sh
npm test
```

## Operator plane

`/admin/*` is day-2 parity with the Go `abbs admin` CLI — same trust plane
(deploy-time secrets), deliberately outside `/spec`; the conformance suite
never touches it. Disabled entirely unless the `OPERATOR_TOKEN` secret is
set; authenticate with `Authorization: Bearer <OPERATOR_TOKEN>`:

| Route | Go CLI equivalent |
|---|---|
| `POST /admin/users {username, kind?, display_name?, admin?}` → `{user, token}` | `abbs admin create-user` |
| `POST /admin/users/{username}/grant` | `abbs admin grant` |
| `POST /admin/users/{username}/revoke` | `abbs admin revoke` |
| `POST /admin/users/{username}/rotate-key` → `{token}` | `abbs admin rotate-key` |

## Layout

Mirrors the Go server (`server` vs `store` split, one store file per domain)
so a reviewer can diff side-by-side: `src/handlers/` ↔ the Go handlers,
`src/store/` ↔ `internal/store/`, `src/middleware.ts` ↔
`internal/server/middleware.go`, `src/emoji.ts` ↔ `internal/emoji/`.

Two deliberate schema deltas from the Go DDL (see PLAN.md "Schema port"):
`idempotency.created_ns` (UnixNano) became `created_ms` — nanoseconds exceed
2^53 and would corrupt the retention comparison as a JS number — and
`idempotency.body` is TEXT, since replay stores and returns the exact JSON
string. Sequence numbers as JS numbers are safe below 2^53, unreachable
within the 10 GB DO storage cap.

## Diff diary — what the spec under-specified

Where this port needed the reference implementation (not the spec) to decide
behavior. Each is a candidate **additive** spec clarification; none is
currently observable by the conformance suite, which is exactly why they are
worth pinning:

1. **Method mismatch is 404, never 405.** The reference mux wrapper returns a
   404 `not-found` problem for any unmatched (method, path) pair. A third
   party would plausibly return 405. Already flagged in PLAN.md; the spec
   should state it.
2. **Idempotency scope is the route *template*, not the URL.** "Per
   principal, per endpoint" doesn't define *endpoint*. The reference scopes
   keys by route pattern (`POST /v1/threads/{thread_id}/messages`), so the
   same key on two different threads is one scope. The spec should define
   endpoint = route template.
3. **Present-but-empty `participants: []` is a validation error**, not a
   public thread. The reference distinguishes absent (public) from present
   (DM ceremony, which then requires ≥1 non-creator participant). Worth a
   sentence in the spec.
4. **Request bodies are a single JSON document, strictly typed.** The spec
   doesn't say what to do with type-mismatched fields (`"title": 123`) or
   trailing garbage after the document. Both implementations 400 with
   `validation` on mismatched types; they differ invisibly on trailing
   garbage (Go's streaming decoder ignores it, `JSON.parse` rejects it).
5. **Cursor token range.** Cursors are "opaque", but the reference emits
   decimal int64. A JS implementation cannot faithfully compare tokens above
   2^53 and must reject them as invalid (this one 400s; Go accepts up to
   int64 max). Bounding tokens ("decimal digits, at most 19 characters")
   would make the edge explicit.
6. **Rate-limit identity for unauthenticated claims.** The reference charges
   `POST /v1/users` under first-claim to a synthetic `claim:<username>`
   principal, which also makes idempotency keys work pre-auth. Worth
   documenting as the expected accounting.

One workerd-specific implementation note (not a spec issue): a Durable
Object must **fully drain every forwarded request body before responding** —
early responses (404s, auth failures) with an unconsumed streaming body crash
the runtime's request pipe. This server drains up-front, discarding bytes
past the 1 MiB cap (the same cap as the Go server's `MaxBytesReader`).
