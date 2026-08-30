import type { AppContext } from "../context";
import { authenticate } from "./middleware";
import { readJsonBody, jsonResponse, MAX_VAULT_BODY_BYTES } from "../http";

// parseKdfParams turns the stored kdf_params column back into the object
// every client expects on the wire. handlePutVault stores it JSON.stringify'd
// (see the comment there for why), so VaultRow.kdf_params is a string in
// D1 and always has been -- but every client-side struct across every
// language this Worker has ever talked to (see account.EncryptedVault in the
// Go client) declares kdf_params as an object, because that's what belongs
// on the wire: it's a nested JSON value, not a client-supplied opaque blob
// like ciphertext. Handing back the stored string verbatim was a real bug,
// caught only once a Go client tried to unmarshal a string into a struct
// field and failed with an error no one signing in could act on.
//
// This does not change what's in D1. The stored representation is still a
// plain string column -- re-serialising on the way in, per handlePutVault's
// comment, keeps its shape dictated by the server rather than by whatever
// JSON a client happened to send. Only the *response* shape changes, by
// parsing that string back into an object right before it goes on the wire.
// That also means an already-broken or foreign row does not need a
// migration: every read heals itself.
//
// Defensive on both directions a stored value could go wrong: a row written
// before this fix (or by some future code path) might already hold an
// object rather than a string, and a hand-edited or corrupted row might
// hold a string that isn't valid JSON at all. Neither should 500 the
// request -- the salt/ciphertext/nonce are still good and worth returning;
// a client that can't make sense of kdf_params will say so on its own.
function parseKdfParams(stored: unknown): unknown {
  if (typeof stored !== "string") return stored;
  try {
    return JSON.parse(stored);
  } catch {
    return stored;
  }
}

export async function handleGetVault(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const vault = await ctx.storage.getVault(auth);
  if (!vault) return jsonResponse({ error: "no vault stored" }, 404);
  return jsonResponse({ ...vault, kdf_params: parseKdfParams(vault.kdf_params) }, 200);
}

export async function handlePutVault(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const body = await readJsonBody<{ salt?: unknown; ciphertext?: unknown; nonce?: unknown; kdf_params?: unknown }>(
    request,
    MAX_VAULT_BODY_BYTES,
  );
  if (
    body === null ||
    typeof body.salt !== "string" ||
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
    // The salt is required, not optional. Without it the client cannot derive
    // the key it encrypted with, so a vault stored without one can never be
    // opened by anyone -- including the person who wrote it.
    salt: body.salt,
    ciphertext: body.ciphertext,
    nonce: body.nonce,
    kdf_params: JSON.stringify(body.kdf_params),
    updated_at: ctx.now(),
  });

  return jsonResponse({ ok: true }, 200);
}
