import type { AppContext } from "../context";
import { verifyJWT, type JWTPayload } from "../crypto";
import { jsonResponse } from "../http";

// Returns the authenticated email, or a Response the caller should return
// as-is. A union return type instead of throwing keeps every handler's
// control flow visible at the call site: `if (auth instanceof Response)
// return auth;` reads as exactly what happens.
//
// A session only counts once the account has a PIN and the session was issued
// under the PIN it has now. An account from before PINs gets 403 "pin
// required" on everything until it sets one -- the client answers that by
// asking for a PIN -- and resetting a PIN ends every session issued before.
export async function authenticate(request: Request, ctx: AppContext): Promise<string | Response> {
  const payload = await bearer(request, ctx);
  if (payload === null || payload.scope === "pin") return jsonResponse({ error: "unauthorized" }, 401);

  const pin = await ctx.storage.getPin(payload.sub);
  if (!pin) return jsonResponse({ error: "pin required" }, 403);
  if (payload.pv !== pin.version) return jsonResponse({ error: "unauthorized" }, 401);
  return payload.sub;
}

// bearer reads and checks the token, whatever its scope.
export async function bearer(request: Request, ctx: AppContext): Promise<JWTPayload | null> {
  const header = request.headers.get("authorization") ?? "";
  const match = /^Bearer (.+)$/.exec(header);
  if (!match) return null;
  return verifyJWT(match[1], ctx.jwtSecret);
}
