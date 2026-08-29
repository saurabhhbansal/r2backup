import type { AppContext } from "../context";
import { verifyJWT } from "../crypto";
import { jsonResponse } from "../http";

// Returns the authenticated email, or a Response the caller should return
// as-is. A union return type instead of throwing keeps every handler's
// control flow visible at the call site: `if (auth instanceof Response)
// return auth;` reads as exactly what happens.
export async function authenticate(request: Request, ctx: AppContext): Promise<string | Response> {
  const header = request.headers.get("authorization") ?? "";
  const match = /^Bearer (.+)$/.exec(header);
  if (!match) return jsonResponse({ error: "unauthorized" }, 401);

  const payload = await verifyJWT(match[1], ctx.jwtSecret);
  if (!payload) return jsonResponse({ error: "unauthorized" }, 401);

  return payload.sub;
}
