import type { AppContext } from "../context";
import type { PinRow } from "../storage";
import { constantTimeEqual, keyShare, openKeys, pinVerifier, signJWT, base64UrlDecode } from "../crypto";
import { readJsonBody, jsonResponse, MAX_AUTH_BODY_BYTES } from "../http";
import { bearer } from "./middleware";

// The PIN step, after the email code. See migrations/0004_pin.sql for what is
// stored and why.
//
//   POST /auth/pin        {proof}                     -> {token, key_share}
//   POST /auth/pin/setup  {salt, kdf_params, proof}   -> {token, key_share, legacy_keys?}
//   POST /auth/pin/reset  {salt, kdf_params, proof}   -> {token, key_share}
//
// Each takes the short-lived token /auth/verify hands out. Setup also takes a
// session from before PINs existed, so a computer already signed in can set
// one without a new email code. Reset needs a fresh email code: forgetting the
// PIN means the saved storage keys cannot be opened any more, so they are
// deleted, and every other computer is signed out.

export const PIN_TOKEN_TTL_SECONDS = 10 * 60;
export const SESSION_TTL_SECONDS = 30 * 24 * 60 * 60;

// Wrong PINs in a row before the account is locked, and how long the first
// lock lasts. Each further run of failures locks for twice as long, up to a
// day.
export const PIN_TRIES = 5;
export const PIN_LOCK_SECONDS = 15 * 60;
const PIN_LOCK_MAX_SECONDS = 24 * 60 * 60;

interface PinBody {
  proof?: unknown;
  salt?: unknown;
  kdf_params?: unknown;
}

// A proof is 32 bytes, base64url. Anything else is a malformed request, not a
// wrong PIN, and is not counted against the account.
function validProof(p: unknown): p is string {
  if (typeof p !== "string" || p.length < 40 || p.length > 64) return false;
  try {
    return base64UrlDecode(p).length === 32;
  } catch {
    return false;
  }
}

function validSalt(s: unknown): s is string {
  if (typeof s !== "string" || s.length > 64) return false;
  try {
    return base64UrlDecode(s).length >= 16;
  } catch {
    return false;
  }
}

function validKdf(k: unknown): k is Record<string, unknown> {
  if (typeof k !== "object" || k === null || Array.isArray(k)) return false;
  const o = k as Record<string, unknown>;
  for (const f of ["time", "memory_kib", "threads", "key_len"]) {
    const v = o[f];
    if (typeof v !== "number" || !Number.isInteger(v) || v <= 0) return false;
  }
  return true;
}

async function sessionBody(ctx: AppContext, email: string, version: number): Promise<Record<string, unknown>> {
  const now = ctx.now();
  const token = await signJWT(
    { sub: email, iat: now, exp: now + SESSION_TTL_SECONDS, scope: "full", pv: version },
    ctx.jwtSecret,
  );
  return { token, key_share: await keyShare(ctx.keysSecret, email) };
}

function lockedResponse(lockedUntil: number, now: number): Response {
  return jsonResponse({ error: "too many attempts", retry_after: lockedUntil - now }, 429);
}

export async function handlePinVerify(request: Request, ctx: AppContext): Promise<Response> {
  const payload = await bearer(request, ctx);
  if (payload === null || payload.scope !== "pin") return jsonResponse({ error: "unauthorized" }, 401);
  const body = await readJsonBody<PinBody>(request, MAX_AUTH_BODY_BYTES);
  if (body === null || !validProof(body.proof)) return jsonResponse({ error: "invalid request" }, 400);

  const email = payload.sub;
  const now = ctx.now();
  const row = await ctx.storage.getPin(email);
  if (!row) return jsonResponse({ error: "pin required" }, 403);
  if (row.locked_until > now) return lockedResponse(row.locked_until, now);

  const got = await pinVerifier(ctx.keysSecret, email, body.proof);
  if (!constantTimeEqual(got, row.verifier)) {
    const failures = await ctx.storage.notePinFailure(email);
    if (failures > 0 && failures % PIN_TRIES === 0) {
      const runs = failures / PIN_TRIES;
      const until = now + Math.min(PIN_LOCK_SECONDS * 2 ** (runs - 1), PIN_LOCK_MAX_SECONDS);
      await ctx.storage.setPinLock(email, until);
      return lockedResponse(until, now);
    }
    return jsonResponse({ error: "wrong pin", tries_left: PIN_TRIES - (failures % PIN_TRIES) }, 401);
  }
  if (row.failures > 0 || row.locked_until > 0) await ctx.storage.clearPinFailures(email);
  return jsonResponse(await sessionBody(ctx, email, row.version), 200);
}

async function newPin(ctx: AppContext, email: string, body: PinBody, version: number): Promise<PinRow | null> {
  if (!validProof(body.proof) || !validSalt(body.salt) || !validKdf(body.kdf_params)) return null;
  const row: PinRow = {
    salt: body.salt,
    kdf_params: JSON.stringify(body.kdf_params),
    verifier: await pinVerifier(ctx.keysSecret, email, body.proof),
    version,
    failures: 0,
    locked_until: 0,
    updated_at: ctx.now(),
  };
  await ctx.storage.putPin(email, row);
  return row;
}

export async function handlePinSetup(request: Request, ctx: AppContext): Promise<Response> {
  const payload = await bearer(request, ctx);
  // The PIN-step token, or a session from before PINs (no scope at all). A
  // full session never is: an account that has one already has a PIN.
  if (payload === null || payload.scope === "full") return jsonResponse({ error: "unauthorized" }, 401);
  const body = await readJsonBody<PinBody>(request, MAX_AUTH_BODY_BYTES);
  if (body === null) return jsonResponse({ error: "invalid request" }, 400);

  const email = payload.sub;
  if (await ctx.storage.getPin(email)) return jsonResponse({ error: "pin already set" }, 409);
  const row = await newPin(ctx, email, body, 1);
  if (!row) return jsonResponse({ error: "invalid request" }, 400);

  const out = await sessionBody(ctx, email, row.version);
  // Keys saved before PINs, which the server could read. They go back to the
  // client this once so it can encrypt them itself; the row is deleted when
  // the sealed copy arrives (PUT /keys).
  const old = await ctx.storage.getKeys(email);
  if (old) {
    const plain = await openKeys(old, email, ctx.keysSecret);
    if (plain !== null) {
      try {
        out.legacy_keys = JSON.parse(plain);
      } catch {
        // Damaged: nothing to hand back. The person enters their keys again.
      }
    }
  }
  return jsonResponse(out, 200);
}

export async function handlePinReset(request: Request, ctx: AppContext): Promise<Response> {
  const payload = await bearer(request, ctx);
  if (payload === null || payload.scope !== "pin") return jsonResponse({ error: "unauthorized" }, 401);
  const body = await readJsonBody<PinBody>(request, MAX_AUTH_BODY_BYTES);
  if (body === null) return jsonResponse({ error: "invalid request" }, 400);

  const email = payload.sub;
  const old = await ctx.storage.getPin(email);
  const row = await newPin(ctx, email, body, (old?.version ?? 0) + 1);
  if (!row) return jsonResponse({ error: "invalid request" }, 400);
  // Encrypted under the forgotten PIN: nobody can open them now.
  await ctx.storage.deleteSealedKeys(email);
  await ctx.storage.deleteKeys(email);
  return jsonResponse(await sessionBody(ctx, email, row.version), 200);
}
