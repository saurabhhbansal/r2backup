export function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

// Auth bodies are a handful of bytes ({email, code}); a vault blob is small
// ciphertext (R2 credentials, not user data). Generous headroom, not a
// tight fit -- the point is a ceiling, not a squeeze.
export const MAX_AUTH_BODY_BYTES = 4 * 1024;
export const MAX_VAULT_BODY_BYTES = 64 * 1024;
export const MAX_DEVICE_BODY_BYTES = 4 * 1024;

// Request bodies are size-limited so a client can't hand the Worker an
// unbounded stream and force it to buffer arbitrarily much memory before a
// single byte of validation happens. Checked twice: once cheaply against
// the declared Content-Length (a client can lie about this, so it's only a
// fast path), and again against the bytes actually read.
export async function readJsonBody<T>(request: Request, maxBytes: number): Promise<T | null> {
  const declaredLength = request.headers.get("content-length");
  if (declaredLength !== null && Number(declaredLength) > maxBytes) return null;

  const buf = await request.arrayBuffer();
  if (buf.byteLength > maxBytes) return null;

  try {
    return JSON.parse(new TextDecoder().decode(buf)) as T;
  } catch {
    return null;
  }
}
