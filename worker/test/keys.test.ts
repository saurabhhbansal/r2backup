import { describe, it, expect, beforeEach } from "vitest";
import { handleGetKeys, handlePutKeys, handleDeleteKeys } from "../src/handlers/keys";
import { signJWT, sealKeys } from "../src/crypto";
import { MemoryStorage, FakeMailer } from "./support/memoryStorage";
import type { AppContext } from "../src/context";

const SECRET = "keys-test-jwt";
const KEYS_SECRET = "keys-test-secret";

function makeContext(storage: MemoryStorage, keysSecret = KEYS_SECRET): AppContext {
  return { storage, mailer: new FakeMailer(), jwtSecret: SECRET, keysSecret, now: () => 700 };
}

async function authed(method: string, email: string, body?: unknown, extra: Record<string, unknown> = {}): Promise<Request> {
  const token = await signJWT({ sub: email, iat: 0, exp: 9_999_999_999, scope: "full", pv: 1, ...extra }, SECRET);
  return new Request("https://r2backup.flexpod.cc/keys", {
    method,
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

// Opaque to the server: whatever base64url the client sends.
const SEALED = { ciphertext: "Y2lwaGVydGV4dC1ieXRlcw", nonce: "bm9uY2UtYnl0ZXMtMjQ" };

describe("/keys", () => {
  let storage: MemoryStorage;
  let ctx: AppContext;
  beforeEach(() => {
    storage = new MemoryStorage({ pinned: true });
    ctx = makeContext(storage);
  });

  it("requires authentication", async () => {
    const res = await handleGetKeys(new Request("https://r2backup.flexpod.cc/keys"), ctx);
    expect(res.status).toBe(401);
  });

  it("404s before anything is stored", async () => {
    const res = await handleGetKeys(await authed("GET", "a@b.com"), ctx);
    expect(res.status).toBe(404);
  });

  it("stores the sealed keys as they came and gives them back", async () => {
    const put = await handlePutKeys(await authed("PUT", "a@b.com", { sealed: SEALED }), ctx);
    expect(put.status).toBe(200);
    expect(storage.sealed.get("a@b.com")).toEqual({ ...SEALED, updated_at: 700 });

    const get = await handleGetKeys(await authed("GET", "a@b.com"), ctx);
    expect(get.status).toBe(200);
    expect(await get.json()).toEqual({ sealed: SEALED, updated_at: 700 });
  });

  it("drops the copy the server could read once a sealed one arrives", async () => {
    const legacy = await sealKeys(JSON.stringify({ primary: { drime_token: "t" } }), "a@b.com", KEYS_SECRET);
    await storage.putKeys("a@b.com", { ...legacy, updated_at: 1 });
    await handlePutKeys(await authed("PUT", "a@b.com", { sealed: SEALED }), ctx);
    expect(storage.keys.has("a@b.com")).toBe(false);
  });

  it("keeps accounts apart", async () => {
    await handlePutKeys(await authed("PUT", "a@b.com", { sealed: SEALED }), ctx);
    const res = await handleGetKeys(await authed("GET", "c@d.com"), ctx);
    expect(res.status).toBe(404);
  });

  it("rejects a body that is not sealed keys", async () => {
    const bad = [
      undefined,
      { keys: { primary: {} } },
      { sealed: null },
      { sealed: { ciphertext: "abc" } },
      { sealed: { ciphertext: "not base64url!", nonce: "abc" } },
      { sealed: { ciphertext: 1, nonce: "abc" } },
    ];
    for (const body of bad) {
      const res = await handlePutKeys(await authed("PUT", "a@b.com", body), ctx);
      expect(res.status).toBe(400);
    }
  });

  it("deletes both copies", async () => {
    await handlePutKeys(await authed("PUT", "a@b.com", { sealed: SEALED }), ctx);
    expect((await handleDeleteKeys(await authed("DELETE", "a@b.com"), ctx)).status).toBe(200);
    expect((await handleGetKeys(await authed("GET", "a@b.com"), ctx)).status).toBe(404);
  });

  it("asks for a PIN when the account has none", async () => {
    const res = await handleGetKeys(await authed("GET", "a@b.com"), makeContext(new MemoryStorage()));
    expect(res.status).toBe(403);
    expect(await res.json()).toEqual({ error: "pin required" });
  });

  it("refuses a session from before PINs, and the PIN-step token", async () => {
    for (const extra of [{ scope: undefined, pv: undefined }, { scope: "pin", pv: undefined }, { pv: 2 }]) {
      const res = await handleGetKeys(await authed("GET", "a@b.com", undefined, extra), ctx);
      expect(res.status).toBe(401);
    }
  });
});
