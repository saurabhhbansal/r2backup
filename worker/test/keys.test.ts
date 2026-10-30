import { describe, it, expect, beforeEach } from "vitest";
import { handleGetKeys, handlePutKeys, handleDeleteKeys } from "../src/handlers/keys";
import { signJWT } from "../src/crypto";
import { MemoryStorage, FakeMailer } from "./support/memoryStorage";
import type { AppContext } from "../src/context";

const SECRET = "keys-test-jwt";
const KEYS_SECRET = "keys-test-secret";

function makeContext(storage: MemoryStorage, keysSecret = KEYS_SECRET): AppContext {
  return { storage, mailer: new FakeMailer(), jwtSecret: SECRET, keysSecret, now: () => 700 };
}

async function authed(method: string, email: string, body?: unknown): Promise<Request> {
  const token = await signJWT({ sub: email, iat: 0, exp: 9_999_999_999 }, SECRET);
  return new Request("https://r2backup.flexpod.cc/keys", {
    method,
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

const KEYS = {
  primary: { provider: "drime", drime_token: "tok-123" },
  second: { account_id: "acc", access_key_id: "AK", secret_access_key: "SK", bucket: "b" },
};

describe("/keys", () => {
  let storage: MemoryStorage;
  let ctx: AppContext;
  beforeEach(() => {
    storage = new MemoryStorage();
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

  it("stores the keys encrypted and gives the same object back", async () => {
    const put = await handlePutKeys(await authed("PUT", "a@b.com", { keys: KEYS }), ctx);
    expect(put.status).toBe(200);

    const row = storage.keys.get("a@b.com")!;
    expect(row.ciphertext).not.toContain("tok-123");
    expect(row.ciphertext).not.toContain("SK");
    expect(row.updated_at).toBe(700);

    const get = await handleGetKeys(await authed("GET", "a@b.com"), ctx);
    expect(get.status).toBe(200);
    expect(await get.json()).toEqual({ keys: KEYS, updated_at: 700 });
  });

  it("does not open another account's row", async () => {
    await handlePutKeys(await authed("PUT", "a@b.com", { keys: KEYS }), ctx);
    storage.keys.set("c@d.com", storage.keys.get("a@b.com")!);
    const res = await handleGetKeys(await authed("GET", "c@d.com"), ctx);
    expect(res.status).toBe(404);
  });

  it("treats a changed secret as nothing stored", async () => {
    await handlePutKeys(await authed("PUT", "a@b.com", { keys: KEYS }), ctx);
    const res = await handleGetKeys(await authed("GET", "a@b.com"), makeContext(storage, "another-secret"));
    expect(res.status).toBe(404);
  });

  it("rejects a body that is not an object", async () => {
    for (const keys of [undefined, null, "x", [1, 2]]) {
      const res = await handlePutKeys(await authed("PUT", "a@b.com", { keys }), ctx);
      expect(res.status).toBe(400);
    }
  });

  it("deletes", async () => {
    await handlePutKeys(await authed("PUT", "a@b.com", { keys: KEYS }), ctx);
    expect((await handleDeleteKeys(await authed("DELETE", "a@b.com"), ctx)).status).toBe(200);
    expect((await handleGetKeys(await authed("GET", "a@b.com"), ctx)).status).toBe(404);
  });
});
