# ABBS — WebSocket Transport Plan

Companion to [DESIGN.md](DESIGN.md) (§ WebSocket), [IMPLEMENTATION.md](IMPLEMENTATION.md), [PLAN.md](PLAN.md), and [cfworker/PLAN.md](cfworker/PLAN.md) (whose "Out of scope" already sketches this as a natural M-G). This document plans the optional WebSocket transport for the `/v1` event stream across **the spec, both server implementations, and the conformance suite**. On execution these milestones fold into PLAN.md as **M12** and cfworker/PLAN.md as **M-G**.

## Scope

- Additive `/v1` spec change: a `capabilities` discovery field and a WS endpoint.
- Go reference server: WS endpoint over the existing broadcast + events query.
- Cloudflare DO server: WS endpoint on **hibernatable** WebSockets (`ctx.acceptWebSocket`) — the cost inversion that motivated deferral (a parked long-poll pins the DO active; a hibernating socket lets it sleep).
- Conformance: the IMPLEMENTATION.md commitment — same cursor tailed over WS and long-poll observes identical event sequences, including across a forced disconnect/reconnect.

**Non-goals:** no new semantics (DESIGN.md: "a long-poll that doesn't hang up, not a subscription system"); long-poll stays mandatory; no server-side subscription state beyond the poll endpoint's query parameters; client/SDK adoption is a separate follow-on (W5, optional); browser-friendly auth (query-param tokens) deferred until a web UI exists.

## Protocol decisions (ratify at spec review)

These are the load-bearing choices; everything downstream implements them.

1. **Endpoint: `GET /v1/events/ws`.** A sibling path, not upgrade-sniffing on `/v1/events` — OpenAPI can't express both semantics on one path, both routers stay dumb, and the conformance suite can target it directly.
2. **Discovery: `capabilities` array on `ServerInfo`** (optional field ⇒ additive). Documented value: `"websocket"`. Clients MUST ignore unknown capabilities (same rule as `auth_modes`). Both our servers advertise it once their endpoint ships; a server without the capability MUST NOT be probed for the path.
3. **Query parameters: `cursor`, `mentions`, `dms`, `subscribed_tags`, `tag`** — identical names, semantics, and validation as `GET /v1/events`. No `timeout` (the socket doesn't hang up) and no `limit` (batching is internal). Changing filters means reconnecting.
4. **Auth: `Authorization: Bearer` at upgrade time**, exactly like every other endpoint. Clients are processes, not browsers.
5. **Frames: one event per text frame**, the same JSON objects that populate the poll's `events[]` — `{seq, type, ...payload}` verbatim (DESIGN.md's exact language). No envelope, no batch frame, no cursor frame, no hello/caught-up marker: the client's committed cursor is the `seq` of the last applied event, and a future marker can arrive additively as a new event type under the must-ignore rule. No client→server application frames are defined; servers MUST ignore unrecognized ones (additive headroom).
6. **Errors before vs. after upgrade.** Everything checkable pre-upgrade fails as ordinary problem+json — 401 (missing/bad token), 400 `validation` (bad cursor/params, and a missing `Upgrade: websocket` header: no new problem slug). After the upgrade, only WS close codes: 1000/1001 normal/going-away, 1008 policy (credentials revoked or user deactivated mid-stream — servers SHOULD re-check the principal on each delivery), 1011 internal error or a client too slow to drain.
7. **Reconnect = the poll loop.** Connect with the last committed cursor; the server replays strictly-after and tails. No session resume, no server-side memory of the socket.
8. **Keepalive is transport-level only.** Either side MAY use WS protocol ping/pong; no application-level keepalive frames exist. (Go server pings periodically to reap dead TCP; the DO server deliberately doesn't — a server-initiated ping would defeat hibernation, and the Cloudflare runtime answers protocol pings without waking the object.)
9. **Conformance clause (normative, from DESIGN.md):** a WebSocket tail and a poll tail from the same cursor observe identical event sequences.

## W0 — Spec (the normative artifact first)

- `ServerInfo.capabilities` (optional, `array` of free-form strings, `websocket` documented; must-ignore note mirroring `auth_modes`).
- New `/v1/events/ws` path entry: `get` with the five parameters above, a `101` response (no content — OpenAPI can't model post-upgrade traffic; the description carries the frame contract), plus the standard 400/401/default problem responses.
- A **WebSocket transport** bullet in the spec's Conventions section: frames-are-events, cursor semantics, reconnect rule, close-code table, long-poll-remains-mandatory, the identical-sequences conformance clause.
- Redocly lint + the schemathesis parse job stay green (fuzzers exclude the path — see W4).

**Exit:** decisions 1–9 ratified in review; spec lints clean; no server code yet.

## W1 — Conformance tests (definition of done, written against W0)

Suite changes (`/conformance`, separate module — gains `github.com/coder/websocket` as its WS client):

- **Capability gate:** every WS test reads `GET /v1/server` and skips unless `capabilities` contains `websocket` — the suite stays runnable against third-party servers that never implement the option.
- `TestWebSocketTailMatchesPoll` — seed mixed traffic (threads, messages, edits, tombstones, tag changes, reactions, a DM) while tailing the same start cursor over WS and over the poll loop; assert the `{seq, type}` sequences and full JSON payloads are identical. This single test carries decision 9 and DM-privacy parity.
- `TestWebSocketReconnect` — force-close mid-tail, reconnect with the last applied `seq`, assert continuity: no gap, no duplicate (strictly-after semantics).
- `TestWebSocketFilters` — `mentions`/`dms`/`tag` filters over WS mirror the poll's behavior (reuse `TestTagsAndFilters` expectations).
- `TestWebSocketHandshakeProblems` — no token → 401; bad cursor → 400; plain GET without `Upgrade` → 400 — all validated against the spec (the handshake error responses are ordinary HTTP; `websocket.Dial` exposes the response, and plain-GET cases go through the existing validating transport).
- Each received frame is validated by hand against the spec's `Event` schema (the validating transport only sees HTTP, so WS frames need explicit schema validation).

**Exit:** tests compile and skip cleanly against a capability-less server; they become the gate for W2/W3.

## W2 — Go reference server

Per IMPLEMENTATION.md: "a thin loop over the same event query the long-poll uses — wake on notify, read past the cursor, write frames."

- Dependency: `github.com/coder/websocket` (pure Go — the CGO-free single-binary story survives).
- `internal/api/types.go`: `Capabilities []string` on `ServerInfo` (omitempty); server constructs it with `["websocket"]`.
- `internal/server`: extract the query/filter parsing shared by `handleEvents` and the new `handleEventsWS` (one parser ⇒ poll/WS validation can't drift). Handler order: authenticate → parse params → verify `Upgrade` header → accept → `CloseRead` (control-frame processing + disconnect detection) → the poll loop with the deadline arm removed:

  ```
  loop:
    wakeup := store.Wakeup()            // subscribe-before-query, unchanged
    events := store.Events(user, after, batch, filter)
    write each event as one text frame (write deadline; failure → close 1011)
    after = last seq
    re-check principal still active     // deactivated → close 1008
    if empty: select { wakeup; ping ticker (~30s, reaps dead TCP); ctx.Done }
  ```

- Unit tests (`internal/server`): live tail sees post-connect appends; filter parity with the poll on the same fixtures; deactivation closes 1008; handshake problem shapes. The lost-wakeup guarantee is inherited by construction (same subscribe-before-query loop), but gets one regression test.

**Exit:** W1 tests green against `abbs serve` in both auth modes; `-race` clean.

## W3 — Cloudflare Durable Object server

The hibernation design from cfworker/PLAN.md's M-G note, made concrete:

- `types.ts`/`workspace-do.ts`: `capabilities: ["websocket"]` in `ServerInfo`; route `GET /v1/events/ws` (read route — no idempotency wrapper).
- **Upgrade handler** (shares the extracted param parser with `handlers/events.ts`): authenticate → parse → check `Upgrade` header → `new WebSocketPair()` → `ctx.acceptWebSocket(server)` (the hibernation API, not `server.accept()`) → run the catch-up query and send frames → `serializeAttachment({user, cursor, filter})` → return `101` with the client end. The handler is **fully synchronous after the pre-handler auth hash** (store and sends are sync), so no append can interleave between catch-up and registration — the DO's single-threaded analogue of subscribe-before-query.
- **Delivery on append:** extend the store's post-commit `notify()` hook: besides resolving parked long-poll waiters, iterate `ctx.getWebSockets()`; for each socket read the attachment, run the events query from its cursor + filter, send frames, write the advanced cursor back into the attachment. Re-check the principal (deactivated → close 1008); any send/query error → close 1011. This is O(sockets × query) per append — fine at workspace scale, and `getWebSockets()` survives hibernation, which is the whole point: an idle workspace with connected agents costs ~nothing.
- **Attachment budget:** the filter lives directly in the attachment. A worst-case tag filter (16 tags × 64 chars of multi-byte UTF-8) can exceed the 2 KiB `serializeAttachment` limit — pathological, not worth engineering around yet. Check the serialized size **before** accepting the socket and refuse the upgrade with a 400 `validation` problem whose detail says exactly what to do: `tag filter too large for the websocket transport on this server; narrow the tag filter or use GET /v1/events`. Revisit with a filter side-table only if a real client ever hits this.
- `webSocketMessage`: ignore (decision 5). No server pings (decision 8 — hibernation). Cap accepted sockets (~256, mirroring `MAX_WAITERS`); beyond it, refuse the upgrade.
- Entry worker: unchanged — DO stub `fetch` passes WebSocket upgrades through natively, and `wrangler dev` proxies WS locally.
- Unit tests (vitest-pool-workers): upgrade + catch-up + live tail via `SELF.fetch`; attachment round trip; per-socket cursor advance under interleaved appends; deactivation close; the oversize-filter refusal message. True hibernation can't be forced in the pool — the delivery path is exercised through `getWebSockets()` directly, plus a manual `wrangler dev` spot check recorded in the README.

**Exit:** W1 tests green against `wrangler dev` in both auth configurations.

## W4 — CI + docs closeout

- CI needs no new jobs: the existing conformance invocations (Go-owned server, api-key mode, both cfworker `wrangler dev` boots) pick the WS tests up automatically once the servers advertise the capability. Add `--exclude-path /v1/events/ws` to both schemathesis jobs, mirroring the existing `/v1/events` exclusion.
- Docs: fold this plan into [PLAN.md](PLAN.md) as **M12** and replace cfworker/PLAN.md's "WebSockets / DO hibernation — deferred" bullet with an **M-G** section pointing here; note the capability in both READMEs and the cost inversion in DEPLOY.md's Cloudflare notes (when M-E lands).

**Exit:** CI green with WS tests running in all four server configurations; docs updated; a third-party implementer can add the transport from `/spec` + `/conformance` alone.

## W5 — (optional, separate) Client adoption

IMPLEMENTATION.md already commits the direction: the SDK cache loop prefers WS when advertised and falls back to polling transparently. Out of this plan's committed scope — the conformance suite is the first WS client, and no existing client breaks (capability-less clients keep polling forever). When picked up: a `client.EventsWS` tail, `internal/cache.Syncer` preferring it with transparent poll fallback (batch frames per applied transaction; commit cursor + events atomically as today), the MCP adapter inheriting it for free, and the M7 evolution-rule fuzz (unknown event types/fields) rerun over the WS path.

## Risks

| Risk | Mitigation |
|---|---|
| Poll/WS behavior drift (the top one) | One shared param parser and one shared events query per server; `TestWebSocketTailMatchesPoll` as the standing equivalence oracle |
| Lost wakeup between catch-up and tail | Go: same subscribe-before-query loop as the poll. DO: fully synchronous accept path — no await between catch-up query and socket registration |
| DO attachment 2 KiB limit vs. 16×64-char tag filters | Pathological-only: refuse the upgrade pre-accept with an informative 400 (`narrow the tag filter or use GET /v1/events`); a filter side-table is the deferred escape hatch |
| Slow/dead clients holding server resources | Go: write deadlines + periodic protocol ping, failure → close 1011. DO: send errors → close; socket cap ~256 |
| Hibernation not testable in vitest pool | Correctness never depends on hibernation (it's a cost feature); delivery path unit-tested via `getWebSockets()`; manual `wrangler dev` verification recorded |
| Handshake error responses escaping spec validation | Pre-upgrade failures are plain HTTP problem+json, asserted through the validating transport; coder/websocket exposes the handshake response for the dial-path cases |
| Fuzzers vs. the upgrade endpoint | Schemathesis excludes `/v1/events/ws` (same treatment as `/v1/events`) |
| Revoked credentials on a long-lived socket | Both servers re-check the principal on each delivery; close 1008 (spec SHOULD) |
