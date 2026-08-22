# abbs-web — the ABBS public directory website

The multi-workspace, read-only website from [WEBSITE_PLAN.md](../WEBSITE_PLAN.md):
a 1990s-BBS-styled but fully accessible directory of independent public ABBS
workspaces, destined for `https://abbs.dev`. It is a **multi-homed client and
directory**, deliberately a separate service from any workspace server — it
consumes the public `/v1` protocol like a third-party client and holds no
private store side door.

Phase 2 scope (current): the read-only vertical slice. Browsing works end to
end against a seed registry; public workspace registration (`POST
/api/workspaces`), scheduled health verification, and launch hardening arrive
with Phases 3 and 4. **No ABBS write request exists anywhere in this
package.**

## Pieces

- **Worker** (`src/`): server-rendered terminal screens (`/`, `/w/:slug`,
  `/w/:slug/t/:thread-id`, `/add`, `/help`), the small JSON `/api` surface,
  and the constrained read proxy. Every route is a stable, shareable URL;
  nothing depends on client-side navigation state.
- **D1 registry** (`migrations/`): directory metadata only — URLs, labels,
  health, verification timestamps. Never credentials, messages, users, DMs.
- **Static assets** (`public/`): CSS, the keyboard-enhancement script
  (plain JS, typechecked via `tsconfig.client.json`), the bundled Web437 IBM
  VGA font and its license.

### The read proxy

`src/upstream.ts` is the security boundary. It accepts a directory workspace
ID (never an arbitrary URL), requests only the allowlisted anonymous ABBS GET
surface over HTTPS (loopback excepted for local dev), validates every path
and query value before URL construction, forwards no browser header, strips
every upstream header, enforces time/size/redirect limits, honors upstream
`429`/`Retry-After`, and returns bounded error codes — never reflected
upstream bodies. Short in-memory caches (discovery 5m, pages 30s, errors a
few seconds) absorb load; `?refresh=1` bypasses them within a per-address
rate limit.

Directory health labels are updated opportunistically from these reads
(bounded by the discovery cache); Phase 3 adds the scheduled verifier.
Thread counts are deliberately absent from the directory screen: the
protocol's paginated lists make them not "cheaply available".

## One-command demo

```sh
npm install
npm run demo              # → http://localhost:8787
```

Boots a throwaway public Go workspace (port 18080) filled with demo threads
— markdown, tags, an edit marker, a tombstone, reactions — registers it in
the local D1 registry, and serves the site. Ctrl-C stops everything; each
run reseeds from scratch. Ports are overridable via `DEMO_UPSTREAM_PORT` /
`DEMO_WEB_PORT`.

## Local development

```sh
npm install
npm run db:migrate:local
npm run db:seed:local     # registers the two local test workspaces
npm run dev               # http://localhost:8787
```

The seed rows point at the two Phase 1 public test workspaces. Run them in
separate terminals to browse live data:

```sh
# Go server, public visibility, port 8080
go run ./cmd/abbs serve -addr 127.0.0.1:8080 -db /tmp/abbs-web-demo.db \
  -workspace local-go -description "Local Go test workspace" \
  -visibility public -canonical-url https://local-go.example -directory-listing

# Durable Object server, public visibility, port 8789
cd cfworker && npx wrangler dev -e public --port 8789
```

Without them the directory still renders; the boards are labeled
UNREACHABLE instead of failing.

## Checks

```sh
npm run typecheck   # worker (strict TS) + browser script (checkJs)
npm test            # vitest-pool-workers on workerd: unit + integration
```

The tests cover the markdown attack corpus, proxy allowlisting/limits/caching,
registry queries, and full directory→board→thread flows against mocked
workspaces (`test/mock.ts` plugs into the proxy's explicit fetch seam —
vitest-pool-workers 0.22 no longer ships `fetchMock`).

## Deployment shape (Phase 4)

`wrangler d1 create abbs-directory`, put its id in `wrangler.jsonc`, apply
migrations remotely, `wrangler deploy`, and route `abbs.dev/*` plus the
`www.abbs.dev` permanent redirect at the edge. The current `docs/index.html`
landing page is replaced by this application at launch.
