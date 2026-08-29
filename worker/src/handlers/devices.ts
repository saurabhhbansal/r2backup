import type { AppContext } from "../context";
import { authenticate } from "./middleware";
import { readJsonBody, jsonResponse, MAX_DEVICE_BODY_BYTES } from "../http";

export async function handlePostDevice(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const body = await readJsonBody<{ device_name?: unknown; os?: unknown }>(request, MAX_DEVICE_BODY_BYTES);
  if (body === null || typeof body.device_name !== "string" || typeof body.os !== "string") {
    return jsonResponse({ error: "invalid request" }, 400);
  }

  await ctx.storage.upsertDevice(auth, {
    device_name: body.device_name,
    os: body.os,
    last_seen: ctx.now(),
  });

  return jsonResponse({ ok: true }, 200);
}

export async function handleGetDevices(request: Request, ctx: AppContext): Promise<Response> {
  const auth = await authenticate(request, ctx);
  if (auth instanceof Response) return auth;

  const devices = await ctx.storage.listDevices(auth);
  return jsonResponse({ devices }, 200);
}
