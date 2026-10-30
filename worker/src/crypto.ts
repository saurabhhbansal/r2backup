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

  // A token that isn't well-formed base64url at all (hand-typed garbage, a
  // truncated paste) must fail the same way a validly-formed but wrong one
  // does -- atob() throws on invalid input, so that has to be caught here
  // rather than left to propagate as a 500.
  let signatureBytes: Uint8Array;
  let payload: JWTPayload;
  try {
    signatureBytes = base64UrlDecode(sigPart);
  } catch {
    return null;
  }

  // crypto.subtle.verify does the comparison itself; it is specified and
  // implemented to run in constant time, so there is nothing to hand-roll
  // here the way there is for the OTP hash comparison below.
  const valid = await crypto.subtle.verify("HMAC", key, signatureBytes, encoder.encode(signingInput));
  if (!valid) return null;

  try {
    payload = JSON.parse(decoder.decode(base64UrlDecode(payloadPart))) as JWTPayload;
  } catch {
    return null;
  }
  if (typeof payload.exp !== "number" || typeof payload.sub !== "string") return null;
  if (Date.now() / 1000 >= payload.exp) return null;
  return payload;
}

// --- stored keys ---------------------------------------------------------
//
// The storage keys an account keeps are encrypted at rest with AES-256-GCM.
// The key is SHA-256 of the KEYS_SECRET Worker secret, and the account's
// email is the additional data: a ciphertext moved to another account's row
// fails to open rather than handing that account someone else's keys.

async function keysKey(secret: string): Promise<CryptoKey> {
  const raw = await crypto.subtle.digest("SHA-256", encoder.encode(secret));
  return crypto.subtle.importKey("raw", raw, { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);
}

export interface SealedKeys {
  ciphertext: string; // base64url
  iv: string; // base64url, 12 bytes
}

export async function sealKeys(plaintext: string, email: string, secret: string): Promise<SealedKeys> {
  if (!secret) throw new Error("KEYS_SECRET is not set");
  const iv = new Uint8Array(12);
  crypto.getRandomValues(iv);
  const sealed = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: encoder.encode(email) },
    await keysKey(secret),
    encoder.encode(plaintext),
  );
  return { ciphertext: base64UrlEncode(sealed), iv: base64UrlEncode(iv) };
}

// Returns null for anything that does not open: a wrong secret, a row from
// another account, or a damaged value. The caller treats all of them alike.
export async function openKeys(sealed: SealedKeys, email: string, secret: string): Promise<string | null> {
  if (!secret) return null;
  try {
    const plain = await crypto.subtle.decrypt(
      { name: "AES-GCM", iv: base64UrlDecode(sealed.iv), additionalData: encoder.encode(email) },
      await keysKey(secret),
      base64UrlDecode(sealed.ciphertext),
    );
    return decoder.decode(plain);
  } catch {
    return null;
  }
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
