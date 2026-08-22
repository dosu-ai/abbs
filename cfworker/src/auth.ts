// Credential plumbing (port of the token pieces of internal/server/server.go).
// Tokens are random opaque strings stored hashed; introspection is a DB
// lookup, revocation is immediate (no JWTs — we own the database).

export async function sha256Hex(data: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(data));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// mintToken mints an opaque bearer token and its storage hash:
// "abbs_" + base64url (no padding) of 24 random bytes.
export async function mintToken(): Promise<{ token: string; tokenHash: string }> {
  const raw = crypto.getRandomValues(new Uint8Array(24));
  let bin = "";
  for (const b of raw) bin += String.fromCharCode(b);
  const token = "abbs_" + btoa(bin).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
  return { token, tokenHash: await sha256Hex(token) };
}

// bearerToken extracts the Authorization bearer credential, or null.
export function bearerToken(request: Request): string | null {
  const h = request.headers.get("Authorization") ?? "";
  if (!h.startsWith("Bearer ")) return null;
  const token = h.slice("Bearer ".length);
  return token === "" ? null : token;
}

// timingSafeEqualStr compares two secrets without leaking length or content
// timing: both sides are hashed to fixed length first, so the final
// comparison runs over uncorrelated fixed-size digests.
export async function timingSafeEqualStr(a: string, b: string): Promise<boolean> {
  const [ha, hb] = await Promise.all([sha256Hex(a), sha256Hex(b)]);
  let diff = 0;
  for (let i = 0; i < ha.length; i++) diff |= ha.charCodeAt(i) ^ hb.charCodeAt(i);
  return diff === 0;
}
