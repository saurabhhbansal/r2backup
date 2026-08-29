// Hand-rolled HS256 JWT and OTP primitives instead of a library dependency.
// HS256 is three primitives -- HMAC-SHA256, base64url, JSON -- and Workers
// already expose all three through Web Crypto, so pulling in a JWT package
// would buy nothing but supply-chain surface for code this small.

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function base64UrlEncode(data: ArrayBuffer | Uint8Array): string {
  const bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function base64UrlDecode(input: string): Uint8Array {
  const normalized = input.replace(/-/g, "+").replace(/_/g, "/");
  const pad = (4 - (normalized.length % 4)) % 4;
  const binary = atob(normalized + "=".repeat(pad));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

function hmacKey(secret: string): Promise<CryptoKey> {
  return crypto.subtle.importKey(
    "raw",
    encoder.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign", "verify"],
  );
}

export interface JWTPayload {
  sub: string; // normalised email
  iat: number; // seconds since epoch
  exp: number; // seconds since epoch
}

// Signed with a secret from a Worker binding (env.JWT_SECRET), never a
// literal in source -- anyone who can read this file must not be able to
// mint a session for an arbitrary email.
export async function signJWT(payload: JWTPayload, secret: string): Promise<string> {
  const header = { alg: "HS256", typ: "JWT" };
  const signingInput = `${base64UrlEncode(encoder.encode(JSON.stringify(header)))}.${base64UrlEncode(
    encoder.encode(JSON.stringify(payload)),
  )}`;
  const key = await hmacKey(secret);
  const signature = await crypto.subtle.sign("HMAC", key, encoder.encode(signingInput));
  return `${signingInput}.${base64UrlEncode(signature)}`;
}

export async function verifyJWT(token: string, secret: string): Promise<JWTPayload | null> {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  const [headerPart, payloadPart, sigPart] = parts;
  const signingInput = `${headerPart}.${payloadPart}`;
  const key = await hmacKey(secret);
  // crypto.subtle.verify does the comparison itself; it is specified and
  // implemented to run in constant time, so there is nothing to hand-roll
  // here the way there is for the OTP hash comparison below.
  const valid = await crypto.subtle.verify("HMAC", key, base64UrlDecode(sigPart), encoder.encode(signingInput));
  if (!valid) return null;

  let payload: JWTPayload;
  try {
    payload = JSON.parse(decoder.decode(base64UrlDecode(payloadPart))) as JWTPayload;
  } catch {
    return null;
  }
  if (typeof payload.exp !== "number" || typeof payload.sub !== "string") return null;
  if (Date.now() / 1000 >= payload.exp) return null;
  return payload;
}

// Six digits from a CSPRNG, not Math.random -- this code is, briefly, the
// only thing standing between an attacker who knows the email and someone
// else's R2 credentials.
export function generateCode(): string {
  const buf = new Uint32Array(1);
  crypto.getRandomValues(buf);
  return String(buf[0] % 1_000_000).padStart(6, "0");
}

export async function hashCode(code: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", encoder.encode(code));
  return base64UrlEncode(digest);
}

// A plain `===` comparison on the hash would leak how many leading bytes
// matched through response-time differences, letting an attacker recover
// the hash (and from it, with enough tries, brute-force the 6-digit code
// space) byte by byte. Both inputs here are fixed-length SHA-256 digests --
// their length is a property of the algorithm, never of the secret -- so
// checking it up front leaks nothing before the constant-time XOR pass.
export function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}
