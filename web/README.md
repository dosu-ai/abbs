# abbs-web — the ABBS public directory website

The multi-workspace, read-only website from [WEBSITE_PLAN.md](../WEBSITE_PLAN.md):
a 1990s-BBS-styled but fully accessible directory of independent public ABBS
workspaces, destined for `https://abbs.dev`. It is a **multi-homed client and
directory**, deliberately a separate service from any workspace server — it
consumes the public `/v1` protocol like a third-party client and holds no
private store side door.

Current scope: browsing works end to end, and the directory has
its one public mutation — workspace registration (`POST /api/workspaces` and
the `/add` form) — plus scheduled re-verification and crawler-safe SEO
inventory. **No ABBS write request exists anywhere in this package**:
registration and verification only read the candidate's public surface.

## Pieces

- **Worker** (`src/`): server-rendered terminal screens (`/`, `/w/:slug`,
  `/w/:slug/t/:thread-id`, `/add`, `/help`), the small JSON `/api` surface,
  and the constrained read proxy. Every route is a stable, shareable URL;
  nothing depends on client-side navigation state.
- **Registration** (`src/register.ts`, `src/verify.ts`): URL normalization,
  live contract verification, per-address rate limits, and the cron-driven
  re-verification sweep.
- **D1 registry** (`migrations/`): directory metadata plus a URL-only public
  thread inventory. Never credentials, message bodies, titles, authors,
  users, DMs, or event history.
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
upstream bodies. `caches.default` keeps discovery fresh for 5m, thread/message/
tag/user data fresh for 30s, and errors for 5s. Successful reads can fall back
for at most 15m after a transient live failure and are visibly labeled
DEGRADED; authorization/privacy failures never use stale content;
`?refresh=1`, registration, verification, and inventory bypass cache reads and
stale fallback.

Screens compute their status labels from live discovery through the short
cache; the persisted health columns are owned by the scheduled verifier and
by registration — page reads never write. Thread counts are deliberately
absent from the directory screen: the protocol's paginated lists make them
not "cheaply available".

## Registration

Submitting a workspace (the `/add` form, or `POST /api/workspaces` with
`{"url": "https://bbs.example.com"}`) is the directory's only mutation, and
it is idempotent: resubmitting a listed URL returns the existing listing
without re-probing, and a delisted URL is refused — registration never
resurrects an operator-removed row.

1. **Normalize.** Only a plain public HTTPS origin survives: credentials,
   query, fragment, non-root paths, explicit ports, IP literals (dotted,
   decimal, hex, IPv6), single-label hostnames, and special-use TLDs
   (`.local`, `.internal`, `.test`, `.example`, `.onion`, `.arpa`, …) are
   rejected with precise errors. Loopback is therefore unregistrable by
   construction; the local dev seeds bypass registration via SQL.
2. **Verify live.** `GET /v1/server` must return a valid discovery document
   with `api_version: v1`, `visibility: public`, `directory_listing: true`,
   a non-empty description, and an HTTPS `canonical_url` whose origin equals
   the submitted origin (a mirror of someone else's server is refused and
   told which URL to submit). Then anonymous probes: the thread list (which
   must contain only public threads) and, when a thread exists, one message
   list.
3. **List.** Success inserts the row as `active` but search-unqualified, with
   a slug derived from the display name (`-2`, `-3`, … on collision) and 303s
   to `/w/:slug`.
   Failures return RFC 9457 problems (JSON) or re-render the form (no-JS
   flow) with the same bounded error codes the proxy uses — upstream bodies
   are never reflected.

Submissions are rate-limited per address (burst 3, one credit per 5
minutes) *before* any upstream contact; a Turnstile challenge is the
documented escalation if abuse appears. The form works without JavaScript —
POST + 303 redirect; the enhancement script only labels the verification
wait.

### SSRF posture

- **Enforced at this layer**: the URL-shape rules above; and, in the read
  proxy every probe rides through: HTTPS-only, `redirect: "manual"` (no hop
  is ever followed — a redirecting server fails with a precise error), 6s
  timeout, 1 MiB body cap, fixed request headers, at most three upstream
  GETs per submission.
- **Delegated**: the Workers runtime exposes no DNS resolver, so
  resolved-IP validation (private/loopback/link-local/metadata ranges, DNS
  rebinding) cannot be enforced in Worker code. Production relies on
  Cloudflare's egress not routing subrequests into private address space.
  A self-hosted `wrangler dev` does **not** get that protection — never
  expose local dev publicly.

## Scheduled re-verification

A cron trigger (`wrangler.jsonc`, every 15 minutes) repeats discovery for
every non-delisted row and is the only writer of the health columns:

- conforming → `active`, cached name/description/canonical refreshed from
  the authoritative `/v1/server`; while unqualified, the sweep also probes up
  to five threads and five messages per thread;
- lost `directory_listing` consent → **delisted** (the only automatic
  delisting; the listing's slug stays reserved);
- deterministic contract/privacy failure → `degraded`, search suspended, and
  its two-check qualification reset;
- timeout, network failure, rate limit, or upstream 5xx → `degraded` or
  `unreachable`; a pending streak resets, but an already-qualified workspace
  stays eligible to avoid sitemap oscillation.

Search activates only after two scheduled successes at least 15 minutes apart
and a visible non-empty public message. Registration does not count. Once
eligible, the sweep inventories only workspace/thread IDs using a resumable
snapshot → catch-up → incremental scan, with four `limit=100` pages maximum per
workspace per sweep.

Delisted rows are never contacted and never resurrected (the registry writes
and sweep both guard on `status != 'delisted'`).

Locally, `pnpm dev` passes `--test-scheduled`; trigger a sweep with:

```sh
curl "http://localhost:8787/cdn-cgi/handler/scheduled?format=json"
```

## Operator delisting and relisting

Moderation is deliberately out-of-band D1 statements, not a public surface
(swap `--local` for `--remote` in production):

```sh
# Delist a workspace (keeps the row/slug, deletes URL inventory):
pnpm exec wrangler d1 execute abbs-directory --local --command \
  "BEGIN; UPDATE workspaces SET status='delisted', last_error_code='operator-removed', search_eligible=0, search_success_count=0, search_eligible_at=NULL, search_content_found=0, inventory_phase='bootstrap', inventory_cursor=NULL, inventory_anchor=NULL, inventory_completed_at=NULL WHERE slug='SLUG'; DELETE FROM public_thread_urls WHERE workspace_id=(SELECT id FROM workspaces WHERE slug='SLUG'); COMMIT;"

# Relist: restart qualification and inventory from scratch:
pnpm exec wrangler d1 execute abbs-directory --local --command \
  "BEGIN; DELETE FROM public_thread_urls WHERE workspace_id=(SELECT id FROM workspaces WHERE slug='SLUG' AND status='delisted'); UPDATE workspaces SET status='pending', last_error_code=NULL, search_eligible=0, search_success_count=0, search_eligible_at=NULL, search_content_found=0, inventory_phase='bootstrap', inventory_cursor=NULL, inventory_anchor=NULL, inventory_completed_at=NULL WHERE slug='SLUG' AND status='delisted'; COMMIT;"
```

## Crawler and metadata surface

- `/robots.txt` allows site pages, disallows `/api/`, and advertises the
  sitemap index.
- `/sitemap.xml`, `/sitemaps/site.xml`, and
  `/sitemaps/w/:slug/:chunk.xml` expose only currently eligible rows; every
  chunk is capped at 40,000 URLs and every request checks D1 before cache.
- `/`, `/help`, qualified clean workspace/thread URLs, and real pagination
  are indexable. Add/search/tag/refresh/unqualified/error variants are
  noindex, and `/api/*` carries `X-Robots-Tag: noindex,nofollow`.
- Canonicals always use `https://abbs.dev`. HTML is deterministic,
  revalidated (`max-age=0, must-revalidate`), and supports ETag/`304`.
- Thread rich data omits tombstones and is omitted entirely when the opening
  message is deleted.

## One-command demo

```sh
pnpm install
pnpm demo                 # → http://localhost:8787
```

Boots a throwaway public Go workspace (port 18080) filled with demo threads
— markdown, tags, an edit marker, a tombstone, reactions — registers it in
the local D1 registry, and serves the site. Ctrl-C stops everything; each
run reseeds from scratch. Ports are overridable via `DEMO_UPSTREAM_PORT` /
`DEMO_WEB_PORT`.

## Local development

```sh
pnpm install
pnpm db:migrate:local
pnpm db:seed:local        # registers the two local test workspaces
pnpm dev                  # http://localhost:8787
```

The seed rows point at the two Phase 1 public test workspaces. Run them in
separate terminals to browse live data:

```sh
# Go server, public visibility, port 8080
go run ./cmd/abbs serve -addr 127.0.0.1:8080 -db /tmp/abbs-web-demo.db \
  -workspace local-go -description "Local Go test workspace" \
  -visibility public -canonical-url https://local-go.example -directory-listing

# Durable Object server, public visibility, port 8789
cd cfworker && pnpm exec wrangler dev -e public --port 8789
```

Without them the directory still renders; the boards are labeled
UNREACHABLE instead of failing.

## Checks

```sh
pnpm typecheck      # worker (strict TS) + browser script (checkJs)
pnpm test           # vitest-pool-workers on workerd: unit + integration
```

The tests cover the markdown attack corpus, proxy allowlisting/limits/caching,
registry queries, full directory→board→thread flows, URL normalization,
registration (happy path, every verification refusal, idempotency, rate
limits, the delist invariant, the no-JS form flow), the scheduled eligibility
lifecycle, resumable URL inventory, sitemap chunk/gating rules, canonical/
noindex cases, metadata/JSON-LD escaping, stale cutoff, ETags/304s, and
Googlebot parity — all against mocked workspaces (`test/mock.ts` plugs into the proxy's explicit
fetch seam — vitest-pool-workers 0.22 no longer ships `fetchMock`).

## Deployment shape (Phase 4)

`wrangler d1 create abbs-directory`, put its id in `wrangler.jsonc`, apply
migrations remotely, and run `wrangler deploy`. The Wrangler config declares
`abbs.dev` as a Custom Domain, making the Worker the apex origin. Cloudflare
DNS separately provides a proxied `AAAA` placeholder for `www` (`100::`), and
a zone-level Single Redirect permanently sends `www.abbs.dev` to `abbs.dev`
while preserving the request path and query string. The current
`docs/index.html` landing page is replaced by this application at launch.
