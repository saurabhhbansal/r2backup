import { describe, it, expect, beforeEach } from "vitest";
import { handlePinVerify, handlePinSetup, handlePinReset, PIN_LOCK_SECONDS } from "../src/handlers/pin";
import { handleGetKeys, handlePutKeys } from "../src/handlers/keys";
import { signJWT, verifyJWT, sealKeys, base64UrlEncode } from "../src/crypto";
import { MemoryStorage, FakeMailer } from "./support/memoryStorage";
import type { AppContext } from "../src/context";

const JWT = "pin-test-jwt";
const KEYS_SECRET = "pin-test-keys";
const EMAIL = "pin@example.com";
const KDF = { time: 3, memory_kib: 65536, threads: 4, key_len: 64 };
const SALT = base64UrlEncode(new Uint8Array(16).fill(7));

function proof(fill: number): string {
  return base64UrlEncode(new Uint8Array(32).fill(fill));
}

async function token(claims: Record<string, unknown>): Promise<string> {
  return signJWT({ sub: EMAIL, iat: 0, exp: 9_999_999_999, ...claims } as never, JWT);
}

function post(path: string, tok: string, body: unknown): Request {
  return new Request(`https://r2backup.flexpod.cc${path}`, {
    method: "POST",
    headers: { authorization: `Bearer ${tok}`, "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

function get(path: string, tok: string): Request {
  return new Request(`https://r2backup.flexpod.cc${path}`, { headers: { authorization: `Bearer ${tok}` } });
}

describe("PIN", () => {
  let storage: MemoryStorage;
  let clock: { now: number };
  let ctx: AppContext;
  let pinTok: string;

  beforeEach(async () => {
    storage = new MemoryStorage();
    clock = { now: Math.floor(Date.now() / 1000) };
    ctx = { storage, mailer: new FakeMailer(), jwtSecret: JWT, keysSecret: KEYS_SECRET, now: () => clock.now };
    pinTok = await token({ scope: "pin" });
  });

  async function setUp(p = proof(1)): Promise<Record<string, unknown>> {
    const res = await handlePinSetup(post("/auth/pin/setup", pinTok, { salt: SALT, kdf_params: KDF, proof: p }), ctx);
    expect(res.status).toBe(200);
    return (await res.json()) as Record<string, unknown>;
  }

  it("sets a PIN and starts a session under it", async () => {
    const out = await setUp();
    const payload = await verifyJWT(out.token as string, JWT);
    expect(payload?.scope).toBe("full");
    expect(payload?.pv).toBe(1);
    expect(typeof out.key_share).toBe("string");

    const row = storage.pins.get(EMAIL)!;
    expect(row.salt).toBe(SALT);
    expect(JSON.parse(row.kdf_params)).toEqual(KDF);
    expect(row.verifier).not.toBe(proof(1));

    // The session works on everything else.
    expect((await handleGetKeys(get("/keys", out.token as string), ctx)).status).toBe(404);
  });

  it("will not set a second PIN over the first", async () => {
    await setUp();
    const res = await handlePinSetup(post("/auth/pin/setup", pinTok, { salt: SALT, kdf_params: KDF, proof: proof(2) }), ctx);
    expect(res.status).toBe(409);
  });

  it("refuses malformed setups without storing anything", async () => {
    const bad = [
      { salt: SALT, kdf_params: KDF },
      { salt: SALT, kdf_params: KDF, proof: "short" },
      { salt: "c2FsdA", kdf_params: KDF, proof: proof(1) },
      { salt: SALT, kdf_params: { time: 3 }, proof: proof(1) },
      { salt: SALT, kdf_params: [1], proof: proof(1) },
    ];
    for (const body of bad) {
      const res = await handlePinSetup(post("/auth/pin/setup", pinTok, body), ctx);
      expect(res.status).toBe(400);
    }
    expect(storage.pins.size).toBe(0);
  });

  it("lets a session from before PINs set one, and hands back the keys the server could read", async () => {
    const legacy = { primary: { provider: "drime", drime_token: "tok" } };
    await storage.putKeys(EMAIL, { ...(await sealKeys(JSON.stringify(legacy), EMAIL, KEYS_SECRET)), updated_at: 1 });
    const old = await token({});
    const res = await handlePinSetup(post("/auth/pin/setup", old, { salt: SALT, kdf_params: KDF, proof: proof(1) }), ctx);
    expect(res.status).toBe(200);
    const out = (await res.json()) as Record<string, unknown>;
    expect(out.legacy_keys).toEqual(legacy);
    // Still there until the sealed copy arrives.
    expect(storage.keys.has(EMAIL)).toBe(true);
    await handlePutKeys(
      new Request("https://r2backup.flexpod.cc/keys", {
        method: "PUT",
        headers: { authorization: `Bearer ${out.token}`, "content-type": "application/json" },
        body: JSON.stringify({ sealed: { ciphertext: "abc", nonce: "def" } }),
      }),
      ctx,
    );
    expect(storage.keys.has(EMAIL)).toBe(false);
  });

  it("does not take a full session for setup", async () => {
    const full = await token({ scope: "full", pv: 1 });
    const res = await handlePinSetup(post("/auth/pin/setup", full, { salt: SALT, kdf_params: KDF, proof: proof(1) }), ctx);
    expect(res.status).toBe(401);
  });

  it("checks the PIN and hands out the same key share every time", async () => {
    const first = await setUp();
    const res = await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(1) }), ctx);
    expect(res.status).toBe(200);
    const out = (await res.json()) as Record<string, unknown>;
    expect(out.key_share).toBe(first.key_share);
    expect((await verifyJWT(out.token as string, JWT))?.pv).toBe(1);
  });

  it("only accepts the PIN-step token", async () => {
    await setUp();
    for (const claims of [{}, { scope: "full", pv: 1 }]) {
      const res = await handlePinVerify(post("/auth/pin", await token(claims), { proof: proof(1) }), ctx);
      expect(res.status).toBe(401);
    }
  });

  it("says when the account has no PIN yet", async () => {
    const res = await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(1) }), ctx);
    expect(res.status).toBe(403);
  });

  it("locks after five wrong PINs, then for longer after five more", async () => {
    await setUp();
    for (let i = 1; i <= 4; i++) {
      const res = await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(9) }), ctx);
      expect(res.status).toBe(401);
      expect(((await res.json()) as { tries_left: number }).tries_left).toBe(5 - i);
    }
    const fifth = await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(9) }), ctx);
    expect(fifth.status).toBe(429);
    expect(((await fifth.json()) as { retry_after: number }).retry_after).toBe(PIN_LOCK_SECONDS);

    // The right PIN does not get through a lock.
    expect((await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(1) }), ctx)).status).toBe(429);

    clock.now += PIN_LOCK_SECONDS;
    for (let i = 0; i < 4; i++) await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(9) }), ctx);
    const tenth = await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(9) }), ctx);
    expect(tenth.status).toBe(429);
    expect(((await tenth.json()) as { retry_after: number }).retry_after).toBe(2 * PIN_LOCK_SECONDS);

    clock.now += 2 * PIN_LOCK_SECONDS;
    expect((await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(1) }), ctx)).status).toBe(200);
    expect(storage.pins.get(EMAIL)!.failures).toBe(0);
  });

  it("does not count a malformed proof as a wrong PIN", async () => {
    await setUp();
    const res = await handlePinVerify(post("/auth/pin", pinTok, { proof: "x" }), ctx);
    expect(res.status).toBe(400);
    expect(storage.pins.get(EMAIL)!.failures).toBe(0);
  });

  it("reset replaces the PIN, deletes the keys it locked, and signs every computer out", async () => {
    const first = await setUp();
    await storage.putSealedKeys(EMAIL, { ciphertext: "a", nonce: "b", updated_at: 1 });
    const res = await handlePinReset(post("/auth/pin/reset", pinTok, { salt: SALT, kdf_params: KDF, proof: proof(3) }), ctx);
    expect(res.status).toBe(200);
    const out = (await res.json()) as Record<string, unknown>;
    expect((await verifyJWT(out.token as string, JWT))?.pv).toBe(2);
    expect(storage.sealed.has(EMAIL)).toBe(false);

    expect((await handleGetKeys(get("/keys", first.token as string), ctx)).status).toBe(401);
    expect((await handleGetKeys(get("/keys", out.token as string), ctx)).status).toBe(404);
    expect((await handlePinVerify(post("/auth/pin", pinTok, { proof: proof(3) }), ctx)).status).toBe(200);
  });

  it("reset needs a fresh email code, not a session", async () => {
    const first = await setUp();
    const res = await handlePinReset(
      post("/auth/pin/reset", first.token as string, { salt: SALT, kdf_params: KDF, proof: proof(3) }),
      ctx,
    );
    expect(res.status).toBe(401);
  });
});
