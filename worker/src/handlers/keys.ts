import type { AppContext } from "../context";
import { authenticate } from "./middleware";
import { readJsonBody, jsonResponse, MAX_VAULT_BODY_BYTES } from "../http";
import { sealKeys, openKeys } from "../crypto";

// The storage keys an account keeps, so a computer signed in with an email
// code gets them without typing them again. The body is whatever JSON object
// the client sends (today: the primary storage and the second copy); the
// server checks only that it is an object of sane size, encrypts it, and
// hands the same object back. See migrations/0003_stored_keys.sql for what
// protects it at rest.

export async function handleGetKeys(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const row = await ctx.storage.getKeys(auth);
  if (!row) return jsonResponse({ error: "no keys stored" }, 404);
  const plain = await openKeys(row, auth, ctx.keysSecret);
  if (plain === null) {
    // A rotated secret or a damaged row. Said as "nothing stored": the
    // client's answer is the same either way -- enter the keys once more.
    console.error("stored keys did not open");
    return jsonResponse({ error: "no keys stored" }, 404);
  }
  let keys: unknown;
  try {
    keys = JSON.parse(plain);
  } catch {
    return jsonResponse({ error: "no keys stored" }, 404);
  }
  return jsonResponse({ keys, updated_at: row.updated_at }, 200);
}

export async function handlePutKeys(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const body = await readJsonBody<{ keys?: unknown }>(request, MAX_VAULT_BODY_BYTES);
  if (body === null || typeof body.keys !== "object" || body.keys === null || Array.isArray(body.keys)) {
    return jsonResponse({ error: "invalid request" }, 400);
  }
  const sealed = await sealKeys(JSON.stringify(body.keys), auth, ctx.keysSecret);
  await ctx.storage.putKeys(auth, { ...sealed, updated_at: ctx.now() });
  return jsonResponse({ ok: true }, 200);
}

export async function handleDeleteKeys(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;
  await ctx.storage.deleteKeys(auth);
  return jsonResponse({ ok: true }, 200);
}
