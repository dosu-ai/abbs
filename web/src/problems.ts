// RFC 9457 problem+json for the website's own /api surface — the same error
// shape the protocol uses (and the same house style as cfworker), with
// website-local problem types. Upstream failures become typed local errors;
// upstream bodies are never reflected.

const PROBLEM_BASE = "https://abbs.dev/problems/";

const problemTitles: Record<string, string> = {
  validation: "Malformed request",
  "not-found": "No such resource",
  "method-not-allowed": "Method not allowed",
  "upstream-unreachable": "Workspace unreachable",
  "upstream-degraded": "Workspace responded unexpectedly",
  "upstream-rate-limited": "Workspace rate limited the directory",
  internal: "Internal error",
};

export function problemResponse(
  status: number,
  slug: string,
  detail: string,
  headers?: Record<string, string>,
): Response {
  const body = {
    type: PROBLEM_BASE + slug,
    title: problemTitles[slug] ?? slug,
    status,
    ...(detail !== "" ? { detail } : {}),
  };
  return new Response(JSON.stringify(body) + "\n", {
    status,
    headers: {
      "Content-Type": "application/problem+json",
      "X-Content-Type-Options": "nosniff",
      "Cache-Control": "no-store",
      ...(headers ?? {}),
    },
  });
}

export function jsonResponse(
  status: number,
  v: unknown,
  headers?: Record<string, string>,
): Response {
  return new Response(JSON.stringify(v) + "\n", {
    status,
    headers: {
      "Content-Type": "application/json",
      "X-Content-Type-Options": "nosniff",
      ...(headers ?? {}),
    },
  });
}
