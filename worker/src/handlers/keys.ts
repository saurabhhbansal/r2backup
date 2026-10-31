import type { AppContext } from "../context";
import { authenticate } from "./middleware";
import { readJsonBody, jsonResponse, MAX_VAULT_BODY_BYTES } from "../http";

// The storage keys an account keeps, so a computer signed in with an email
// code and PIN gets them without typing them again.
//
// They arrive encrypted by the client, under a key made from the PIN and the
// share POST /auth/pin hands out (see migrations/0004_pin.sql). The server
// checks the shape and stores the bytes; it cannot open them.
//
//   GET    /keys  -> {sealed: {ciphertext, nonce}, updated_at}
//   PUT    /keys  {sealed: {ciphertext, nonce}}
//   DELETE /keys

const MAX_FIELD = 48 * 1024;

function validField(v: unknown): v is string {
  return typeof v === "string" && v.length > 0 && v.length <= MAX_FIELD && /^[A-Za-z0-9_-]+$/.test(v);
}

export async function handleGetKeys(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const row = await ctx.storage.getSealedKeys(auth);
  if (!row) return jsonResponse({ error: "no keys stored" }, 404);
  return jsonResponse({ sealed: { ciphertext: row.ciphertext, nonce: row.nonce }, updated_at: row.updated_at }, 200);
}

export async function handlePutKeys(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const body = await readJsonBody<{ sealed?: { ciphertext?: unknown; nonce?: unknown } }>(
    request,
    MAX_VAULT_BODY_BYTES,
  );
  const sealed = body?.sealed;
  if (!sealed || typeof sealed !== "object" || !validField(sealed.ciphertext) || !validField(sealed.nonce)) {
    return jsonResponse({ error: "invalid request" }, 400);
  }
  await ctx.storage.putSealedKeys(auth, { ciphertext: sealed.ciphertext, nonce: sealed.nonce, updated_at: ctx.now() });
  // The copy from before PINs, which the server could read, goes the moment
  // one it cannot read takes its place.
  await ctx.storage.deleteKeys(auth);
  return jsonResponse({ ok: true }, 200);
}

export async function handleDeleteKeys(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;
  await ctx.storage.deleteSealedKeys(auth);
  await ctx.storage.deleteKeys(auth);
  return jsonResponse({ ok: true }, 200);
}
