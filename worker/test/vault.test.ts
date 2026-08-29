import { describe, it, expect, beforeEach } from "vitest";
import { handleGetVault, handlePutVault } from "../src/handlers/vault";
import { signJWT } from "../src/crypto";
import { MemoryStorage, FakeMailer } from "./support/memoryStorage";
import type { AppContext } from "../src/context";

const SECRET = "vault-test-secret";

function makeContext(storage: MemoryStorage, now: number): AppContext {
  return { storage, mailer: new FakeMailer(), jwtSecret: SECRET, now: () => now };
}

async function authedRequest(method: string, path: string, email: string, body?: unknown): Promise<Request> {
  const token = await signJWT({ sub: email, iat: 0, exp: 9_999_999_999 }, SECRET);
  return new Request(`https://r2backup.flexpod.cc${path}`, {
    method,
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

describe("GET /vault", () => {
  it("requires authentication", async () => {
    const ctx = makeContext(new MemoryStorage(), 100);
    const res = await handleGetVault(new Request("https://r2backup.flexpod.cc/vault"), ctx);
    expect(res.status).toBe(401);
  });

  it("404s when nothing has been stored yet", async () => {
    const ctx = makeContext(new MemoryStorage(), 100);
    const res = await handleGetVault(await authedRequest("GET", "/vault", "a@b.com"), ctx);
    expect(res.status).toBe(404);
  });
});

describe("PUT /vault then GET /vault", () => {
  let storage: MemoryStorage;
  let ctx: AppContext;

  beforeEach(() => {
    storage = new MemoryStorage();
    ctx = makeContext(storage, 500);
  });

  it("stores and returns the blob unchanged, opaque to the server", async () => {
    const blob = {
      ciphertext: "YmFzZTY0Y2lwaGVydGV4dA==",
      nonce: "YmFzZTY0bm9uY2U=",
      kdf_params: { time: 3, memory: 65536, threads: 4, key_len: 32 },
    };
    const putRes = await handlePutVault(await authedRequest("PUT", "/vault", "owner@example.com", blob), ctx);
    expect(putRes.status).toBe(200);

    const getRes = await handleGetVault(await authedRequest("GET", "/vault", "owner@example.com"), ctx);
    expect(getRes.status).toBe(200);
    const stored = (await getRes.json()) as { ciphertext: string; nonce: string; kdf_params: string };
    expect(stored.ciphertext).toBe(blob.ciphertext);
    expect(stored.nonce).toBe(blob.nonce);
    expect(JSON.parse(stored.kdf_params)).toEqual(blob.kdf_params);
  });

  it("keeps vaults isolated per email", async () => {
    await handlePutVault(
      await authedRequest("PUT", "/vault", "alice@example.com", { ciphertext: "a", nonce: "a", kdf_params: {} }),
      ctx,
    );
    const res = await handleGetVault(await authedRequest("GET", "/vault", "bob@example.com"), ctx);
    expect(res.status).toBe(404);
  });

  it("rejects a body missing required fields", async () => {
    const res = await handlePutVault(
      await authedRequest("PUT", "/vault", "owner@example.com", { ciphertext: "only-this" }),
      ctx,
    );
    expect(res.status).toBe(400);
  });

  it("rejects a token signed with the wrong secret", async () => {
    const forged = await signJWT({ sub: "owner@example.com", iat: 0, exp: 9_999_999_999 }, "not-the-real-secret");
    const res = await handleGetVault(
      new Request("https://r2backup.flexpod.cc/vault", { headers: { authorization: `Bearer ${forged}` } }),
      ctx,
    );
    expect(res.status).toBe(401);
  });
});
