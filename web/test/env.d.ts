import type { D1Migration } from "@cloudflare/vitest-pool-workers";
import type { Env as AppEnv } from "../src/types";

// The cloudflare:test module types its `env` as Cloudflare.Env — the
// namespace `wrangler types` would generate. Bind it to our Env shape plus
// the migrations array injected by vitest.config.ts.
declare global {
  namespace Cloudflare {
    interface Env extends AppEnv {
      TEST_MIGRATIONS: D1Migration[];
    }
  }
}

export {};
