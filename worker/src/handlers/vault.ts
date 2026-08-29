import type { AppContext } from "../context";
import { authenticate } from "./middleware";
import { readJsonBody, jsonResponse, MAX_VAULT_BODY_BYTES } from "../http";

export async function handleGetVault(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const vault = await ctx.storage.getVault(auth);
  if (!vault) return jsonResponse({ error: "no vault stored" }, 404);
  return jsonResponse(vault, 200);
}

export async function handlePutVault(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const body = await readJsonBody<{ ciphertext?: unknown; nonce?: unknown; kdf_params?: unknown }>(
    request,
    MAX_VAULT_BODY_BYTES,
  );
  if (
    body === null ||
    typeof body.ciphertext !== "string" ||
    typeof body.nonce !== "string" ||
    body.kdf_params === undefined
  ) {
    return jsonResponse({ error: "invalid request" }, 400);
  }

  // Stored verbatim. The server never inspects what ciphertext/nonce decode
  // to, or what shape the plaintext behind them has -- doing so would mean
  // parsing the credentials, which is exactly what this endpoint exists to
  // never be able to do. kdf_params is the one field that's meaningful to
  // the server's own storage format (not to the plaintext) and is
  // re-serialised so its shape isn't dictated by whatever JSON the client
  // happened to send.
  await ctx.storage.putVault(auth, {
    ciphertext: body.ciphertext,
    nonce: body.nonce,
    kdf_params: JSON.stringify(body.kdf_params),
    updated_at: ctx.now(),
  });

  return jsonResponse({ ok: true }, 200);
}
