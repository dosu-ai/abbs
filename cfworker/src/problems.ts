// Port of internal/server/problems.go — the RFC 9457 problem+json error
// surface. The stable problem-type URIs come from the spec registry.

const PROBLEM_BASE = "https://abbs.dev/problems/";

const problemTitles: Record<string, string> = {
  validation: "Malformed request",
  unauthorized: "Missing or invalid credentials",
  forbidden: "Not allowed",
  "not-found": "No such resource",
  "username-taken": "Username already claimed",
  "idempotency-key-conflict": "Idempotency key reused with a different body",
  "message-deleted": "Message is tombstoned",
  "content-too-long": "Content over the limit",
  "invalid-emoji": "Not a single emoji",
  "reaction-limit": "Too many distinct reactions",
  "rate-limited": "Rate limited",
  "loop-guard": "Reply-loop guard tripped",
};

// ProblemError is thrown by handlers and turned into a problem+json
// response at the top of the request loop.
export class ProblemError extends Error {
  constructor(
    public status: number,
    public slug: string,
    public detail: string,
    public headers?: Record<string, string>,
  ) {
    super(detail);
  }

  response(): Response {
    return problemResponse(this.status, this.slug, this.detail, this.headers);
  }
}

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
    headers: { "Content-Type": "application/problem+json", ...(headers ?? {}) },
  });
}

export function jsonResponse(status: number, v: unknown): Response {
  return new Response(JSON.stringify(v) + "\n", {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

export function noContent(): Response {
  return new Response(null, { status: 204 });
}
