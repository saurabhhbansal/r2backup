import type { AppContext } from "../context";
import { normalizeEmail, isValidEmail } from "../email";
import { generateCode, hashCode, constantTimeEqual, signJWT } from "../crypto";
import { checkRateLimit } from "../ratelimit";
import { readJsonBody, jsonResponse, MAX_AUTH_BODY_BYTES } from "../http";

const OTP_TTL_SECONDS = 10 * 60;
const MAX_ATTEMPTS = 5;
const TOKEN_TTL_SECONDS = 30 * 24 * 60 * 60;

const REQUEST_WINDOW_SECONDS = 10 * 60;
const MAX_REQUESTS_PER_EMAIL = 3;
const MAX_REQUESTS_PER_IP = 10;

// One shape, one status, for every path through this handler: an unknown
// email, an existing one, and a malformed-but-plausible email all end here.
// The only two exits that differ are "the request itself was junk" (400,
// before any lookup happens) and "you've asked too many times" (429, which
// fires identically for an address that has never signed up). Branching on
// whether an *account* exists is the one mistake that would turn a login
// endpoint into a tool for finding out who has one.
const GENERIC_OK = { ok: true };

export async function handleRequestCode(request: Request, ctx: AppContext, clientIp: string): Promise<Response> {
  const body = await readJsonBody<{ email?: unknown }>(request, MAX_AUTH_BODY_BYTES);
  if (body === null || typeof body.email !== "string") {
    return jsonResponse({ error: "invalid request" }, 400);
  }

  const email = normalizeEmail(body.email);
  if (!isValidEmail(email)) {
    return jsonResponse({ error: "invalid request" }, 400);
  }

  const now = ctx.now();
  const emailLimit = await checkRateLimit(
    ctx.storage,
    `email:${email}`,
    now,
    REQUEST_WINDOW_SECONDS,
    MAX_REQUESTS_PER_EMAIL,
  );
  const ipLimit = await checkRateLimit(ctx.storage, `ip:${clientIp}`, now, REQUEST_WINDOW_SECONDS, MAX_REQUESTS_PER_IP);
  if (!emailLimit.allowed || !ipLimit.allowed) {
    return jsonResponse({ error: "too many requests" }, 429);
  }

  const code = generateCode();
  const codeHash = await hashCode(code);
  await ctx.storage.putOtp(email, codeHash, now + OTP_TTL_SECONDS);

  try {
    await ctx.mailer.sendCode(email, code, OTP_TTL_SECONDS / 60);
  } catch {
    // Swallowed on purpose: surfacing "the email failed to send" here would
    // itself be a side channel (a domain with no MX record behaves
    // differently from one Resend rejects for another reason). The stored
    // code and the response are identical either way; only the server log,
    // and only by status, ever sees the difference.
    console.error("sendCode failed");
  }

  return jsonResponse(GENERIC_OK, 200);
}

export async function handleVerify(request: Request, ctx: AppContext): Promise<Response> {
  const body = await readJsonBody<{ email?: unknown; code?: unknown }>(request, MAX_AUTH_BODY_BYTES);
  if (body === null || typeof body.email !== "string" || typeof body.code !== "string") {
    return jsonResponse({ error: "invalid request" }, 400);
  }

  const email = normalizeEmail(body.email);
  const now = ctx.now();

  const otp = await ctx.storage.getOtp(email);
  if (!otp) {
    return jsonResponse({ error: "invalid or expired code" }, 400);
  }
  if (otp.expires_at <= now) {
    await ctx.storage.deleteOtp(email);
    return jsonResponse({ error: "invalid or expired code" }, 400);
  }
  if (otp.attempts >= MAX_ATTEMPTS) {
    // Already dead from a previous request; clean it up so a late retry
    // doesn't find a lingering row and increment attempts past the ceiling.
    await ctx.storage.deleteOtp(email);
    return jsonResponse({ error: "invalid or expired code" }, 400);
  }

  const suppliedHash = await hashCode(body.code);
  if (!constantTimeEqual(suppliedHash, otp.code_hash)) {
    const attempts = await ctx.storage.incrementOtpAttempts(email);
    if (attempts >= MAX_ATTEMPTS) {
      await ctx.storage.deleteOtp(email); // max attempts reached: the code dies
    }
    return jsonResponse({ error: "invalid or expired code" }, 400);
  }

  // Single-use: delete before anything else can observe this code as valid
  // again, including a concurrent request for the same email.
  await ctx.storage.deleteOtp(email);
  await ctx.storage.upsertUser(email, now);

  const token = await signJWT({ sub: email, iat: now, exp: now + TOKEN_TTL_SECONDS }, ctx.jwtSecret);
  return jsonResponse({ token }, 200);
}
