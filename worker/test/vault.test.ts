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
      salt: "YmFzZTY0c2FsdA==",
      ciphertext: "YmFzZTY0Y2lwaGVydGV4dA==",
      nonce: "YmFzZTY0bm9uY2U=",
      kdf_params: { time: 3, memory: 65536, threads: 4, key_len: 32 },
    };
    const putRes = await handlePutVault(await authedRequest("PUT", "/vault", "owner@example.com", blob), ctx);
    expect(putRes.status).toBe(200);

    const getRes = await handleGetVault(await authedRequest("GET", "/vault", "owner@example.com"), ctx);
    expect(getRes.status).toBe(200);
    const stored = (await getRes.json()) as {
      salt: string; ciphertext: string; nonce: string; kdf_params: unknown;
    };
    // The salt has to survive the trip. Without it the client cannot derive
    // the key it encrypted with, so a vault returned without one can never be
    // opened -- by anyone, including whoever wrote it.
    expect(stored.salt).toBe(blob.salt);
    expect(stored.ciphertext).toBe(blob.ciphertext);
    expect(stored.nonce).toBe(blob.nonce);
    // kdf_params comes back as the object it went in as, not the string D1
    // stores it as. This is the one field every client-side struct declares
    // as a nested object rather than an opaque client-supplied blob (see
    // internal/account.EncryptedVault on the Go side), and a Worker that
    // handed the stored string back verbatim broke every one of them on
    // sign-in with no way for a person to act on the error.
    expect(stored.kdf_params).toEqual(blob.kdf_params);
  });

  it("parses kdf_params back into an object even though D1 stores it as a string", async () => {
    // Regression test for the actual production shape: putVault always
    // stringifies kdf_params before it reaches storage (see
    // handlePutVault), so this is what every row in the real vaults table
    // looks like today, confirmed directly against production D1:
    //   kdf_params = "{\"time\":1,\"memory_kib\":65536,\"threads\":4,\"key_len\":32}"
    // A getVault that returned this column unparsed is exactly the bug that
    // shipped: the Go client declares kdf_params as an object field and
    // failed to unmarshal a string into it.
    await storage.putVault("owner@example.com", {
      salt: "YmFzZTY0c2FsdA==",
      ciphertext: "YmFzZTY0Y2lwaGVydGV4dA==",
      nonce: "YmFzZTY0bm9uY2U=",
      kdf_params: JSON.stringify({ time: 1, memory_kib: 65536, threads: 4, key_len: 32 }),
      updated_at: 1788036275,
    });

    const res = await handleGetVault(await authedRequest("GET", "/vault", "owner@example.com"), ctx);
    expect(res.status).toBe(200);
    const body = (await res.json()) as { kdf_params: unknown };
    expect(body.kdf_params).toEqual({ time: 1, memory_kib: 65536, threads: 4, key_len: 32 });
  });

  it("does not 500 when a stored kdf_params is already an object", async () => {
    // Defensive: a row written by some future or foreign code path might
    // not go through handlePutVault's stringify step. getVault's return
    // type says string, but nothing stops a stray row from disagreeing.
    await storage.putVault("owner@example.com", {
      salt: "c2FsdA==",
      ciphertext: "Y2lwaGVy",
      nonce: "bm9uY2U=",
      kdf_params: { time: 1, memory_kib: 65536, threads: 4, key_len: 32 } as unknown as string,
      updated_at: 1,
    });

    const res = await handleGetVault(await authedRequest("GET", "/vault", "owner@example.com"), ctx);
    expect(res.status).toBe(200);
    const body = (await res.json()) as { kdf_params: unknown };
    expect(body.kdf_params).toEqual({ time: 1, memory_kib: 65536, threads: 4, key_len: 32 });
  });

  it("does not 500 when a stored kdf_params is unparseable garbage", async () => {
    await storage.putVault("owner@example.com", {
      salt: "c2FsdA==",
      ciphertext: "Y2lwaGVy",
      nonce: "bm9uY2U=",
      kdf_params: "{not valid json",
      updated_at: 1,
    });

    const res = await handleGetVault(await authedRequest("GET", "/vault", "owner@example.com"), ctx);
    expect(res.status).toBe(200);
    const body = (await res.json()) as { kdf_params: unknown };
    // Handed back verbatim rather than crashing the request. A client will
    // fail to make sense of it on its own -- this row is corrupt in a way
    // that predates this endpoint and no amount of parsing recovers it.
    expect(body.kdf_params).toBe("{not valid json");
  });

  it("keeps vaults isolated per email", async () => {
    await handlePutVault(
      await authedRequest("PUT", "/vault", "alice@example.com", { salt: "s", ciphertext: "a", nonce: "a", kdf_params: {} }),
      ctx,
    );
    const res = await handleGetVault(await authedRequest("GET", "/vault", "bob@example.com"), ctx);
    expect(res.status).toBe(404);
  });

  it("refuses a vault with no salt, which could never be opened", async () => {
    const res = await handlePutVault(
      await authedRequest("PUT", "/vault", "owner@example.com", {
        ciphertext: "YmFzZTY0", nonce: "YmFzZTY0", kdf_params: {},
      }),
      ctx,
    );
    expect(res.status).toBe(400);
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
