import type { Env as AppEnv } from "../src/types";

// The cloudflare:test module types its `env` as Cloudflare.Env — the
// namespace `wrangler types` would generate. Bind it to our Env shape.
declare global {
  namespace Cloudflare {
    interface Env extends AppEnv {}
  }
}

export {};
