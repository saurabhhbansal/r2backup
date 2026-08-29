import { jsonResponse } from "../http";

// Unauthenticated and cheap: no D1 round trip, just proof the Worker is
// alive. Used for uptime checks, so it must never itself be the thing that
// times out or gets rate-limited.
export function handleHealth(): Response {
  return jsonResponse({ ok: true }, 200);
}
