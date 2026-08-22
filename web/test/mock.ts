// A minimal outbound-fetch mock with the undici MockAgent surface our tests
// use. vitest-pool-workers 0.22 removed the cloudflare:test fetchMock, so
// this plugs into upstream.ts's explicit fetch seam instead. Unmatched
// requests reject (the equivalent of disableNetConnect()), which the proxy
// maps to its "network" error.

import { setUpstreamFetchForTests } from "../src/upstream";

interface Reply {
  status: number;
  body: string;
  headers: Record<string, string>;
}

interface Route {
  origin: string;
  path: string;
  query: Record<string, string> | null;
  reply: Reply | null;
  error: Error | null;
  times: number; // Infinity when persisted
  used: number;
}

const routes: Route[] = [];

class Scope {
  constructor(private route: Route) {}
  persist(): Scope {
    this.route.times = Infinity;
    return this;
  }
  times(n: number): Scope {
    this.route.times = n;
    return this;
  }
}

class Interceptor {
  constructor(private route: Route) {}
  reply(status: number, body: string, opts?: { headers?: Record<string, string> }): Scope {
    this.route.reply = { status, body, headers: opts?.headers ?? {} };
    return new Scope(this.route);
  }
  replyWithError(error: Error): Scope {
    this.route.error = error;
    return new Scope(this.route);
  }
}

class Origin {
  constructor(private origin: string) {}
  intercept(opts: { path: string; query?: Record<string, unknown> }): Interceptor {
    const route: Route = {
      origin: this.origin,
      path: opts.path,
      query:
        opts.query === undefined
          ? null
          : Object.fromEntries(Object.entries(opts.query).map(([k, v]) => [k, String(v)])),
      reply: null,
      error: null,
      times: 1,
      used: 0,
    };
    routes.push(route);
    return new Interceptor(route);
  }
}

function queryMatches(route: Route, url: URL): boolean {
  const want = route.query ?? {};
  const got = [...url.searchParams.entries()];
  const wantEntries = Object.entries(want);
  if (got.length !== wantEntries.length) return false;
  return wantEntries.every(([k, v]) => url.searchParams.get(k) === v);
}

async function dispatch(input: string, _init: RequestInit): Promise<Response> {
  const url = new URL(input);
  for (const r of routes) {
    if (r.used >= r.times) continue;
    if (r.origin !== url.origin || r.path !== url.pathname) continue;
    if (!queryMatches(r, url)) continue;
    r.used++;
    if (r.error !== null) throw r.error;
    const reply = r.reply;
    if (reply === null) throw new Error(`interceptor for ${input} has no reply`);
    return new Response(reply.body === "" ? null : reply.body, {
      status: reply.status,
      headers: reply.headers,
    });
  }
  throw new Error(`no mock for outbound request: ${input}`);
}

export const fetchMock = {
  // Wires the mock into the proxy's fetch seam. Idempotent.
  activate(): void {
    setUpstreamFetchForTests(dispatch);
  },
  // Unmatched requests always reject in this mock; kept for API parity.
  disableNetConnect(): void {},
  get(origin: string): Origin {
    return new Origin(origin.replace(/\/$/, ""));
  },
  assertNoPendingInterceptors(): void {
    const pending = routes.filter((r) => r.times !== Infinity && r.used < r.times);
    if (pending.length > 0) {
      const list = pending.map((r) => `${r.origin}${r.path}`).join(", ");
      throw new Error(`pending mock interceptors: ${list}`);
    }
  },
  reset(): void {
    routes.length = 0;
  },
};
