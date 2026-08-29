import type { Env } from "./types";
import type { AppContext } from "./context";
import { D1Storage } from "./storage";
import { ResendMailer } from "./email";
import { handleRequestCode, handleVerify } from "./handlers/auth";
import { handleGetVault, handlePutVault } from "./handlers/vault";
import { handlePostDevice, handleGetDevices } from "./handlers/devices";
import { handleHealth } from "./handlers/health";
import { jsonResponse } from "./http";

function buildContext(env: Env): AppContext {
  return {
    storage: new D1Storage(env.DB),
    mailer: new ResendMailer(env.RESEND_API_KEY),
    jwtSecret: env.JWT_SECRET,
    now: () => Math.floor(Date.now() / 1000),
  };
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    const ctx = buildContext(env);
    // CF-Connecting-IP is set by Cloudflare's edge on every request that
    // reaches a Worker and cannot be spoofed by the client, unlike
    // X-Forwarded-For -- it's the only IP-per-request rate limiting can
    // trust.
    const clientIp = request.headers.get("cf-connecting-ip") ?? "unknown";

    try {
      if (url.pathname === "/health" && request.method === "GET") {
        return handleHealth();
      }
      if (url.pathname === "/auth/request" && request.method === "POST") {
        return await handleRequestCode(request, ctx, clientIp);
      }
      if (url.pathname === "/auth/verify" && request.method === "POST") {
        return await handleVerify(request, ctx);
      }
      if (url.pathname === "/vault" && request.method === "GET") {
        return await handleGetVault(request, ctx);
      }
      if (url.pathname === "/vault" && request.method === "PUT") {
        return await handlePutVault(request, ctx);
      }
      if (url.pathname === "/devices" && request.method === "POST") {
        return await handlePostDevice(request, ctx);
      }
      if (url.pathname === "/devices" && request.method === "GET") {
        return await handleGetDevices(request, ctx);
      }
      return jsonResponse({ error: "not found" }, 404);
    } catch (err) {
      // Never logs `err` verbatim: a JSON parse failure or a validation
      // error can carry the request body in its message, and the request
      // body is precisely where a code, a token, or a vault blob lives.
      console.error("unhandled error:", err instanceof Error ? err.name : "unknown");
      return jsonResponse({ error: "internal error" }, 500);
    }
  },
};
