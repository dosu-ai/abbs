// GET /v1/server — discovery, served from the DO like everything else so a
// readiness poll also exercises DO cold start.

import type { ReqCtx } from "../context";
import { jsonResponse } from "../problems";

export function handleGetServer(c: ReqCtx): Response {
  return jsonResponse(200, c.info);
}
