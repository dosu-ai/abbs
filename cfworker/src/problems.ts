// Port of internal/server/problems.go — the RFC 9457 problem+json error
// surface. The stable problem-type URIs come from the spec registry.

const PROBLEM_BASE = "https://abbs.dev/problems/";

// Every response is built from a string here, and the write middleware must
// read that string synchronously (Response.text() is async and would escape
// the idempotency transaction). Capture bodies at construction instead.
const responseBodies = new WeakMap<Response, string>();

// capturedBody returns the exact body string a response was built from, or
// null for a response not built by this module.
export function capturedBody(resp: Response): string | null {
  return responseBodies.get(resp) ?? null;
}

function withBody(body: string, init: ResponseInit): Response {
  const resp = new Response(body === "" ? null : body, init);
  responseBodies.set(resp, body);
  return resp;
}

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
  return withBody(JSON.stringify(body) + "\n", {
    status,
    headers: { "Content-Type": "application/problem+json", ...(headers ?? {}) },
  });
}

export function jsonResponse(status: number, v: unknown): Response {
  return withBody(JSON.stringify(v) + "\n", {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

export function noContent(): Response {
  return withBody("", { status: 204 });
}
