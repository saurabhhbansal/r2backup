import { describe, it, expect } from "vitest";
import { signJWT, verifyJWT, generateCode, hashCode, constantTimeEqual } from "../src/crypto";

describe("JWT", () => {
  it("round-trips a payload signed with the right secret", async () => {
    const now = Math.floor(Date.now() / 1000);
    const token = await signJWT({ sub: "a@b.com", iat: now, exp: now + 3600 }, "secret-1");
    const payload = await verifyJWT(token, "secret-1");
    expect(payload).not.toBeNull();
    expect(payload?.sub).toBe("a@b.com");
  });

  it("rejects a token signed with a different secret", async () => {
    const now = Math.floor(Date.now() / 1000);
    const token = await signJWT({ sub: "a@b.com", iat: now, exp: now + 3600 }, "secret-1");
    const payload = await verifyJWT(token, "secret-2");
    expect(payload).toBeNull();
  });

  it("rejects an expired token even with the right secret", async () => {
    const now = Math.floor(Date.now() / 1000);
    const token = await signJWT({ sub: "a@b.com", iat: now - 10, exp: now - 1 }, "secret-1");
    const payload = await verifyJWT(token, "secret-1");
    expect(payload).toBeNull();
  });

  it("rejects a token with a tampered payload", async () => {
    const now = Math.floor(Date.now() / 1000);
    const token = await signJWT({ sub: "victim@b.com", iat: now, exp: now + 3600 }, "secret-1");
    const [header, payload, sig] = token.split(".");
    const forgedPayload = Buffer.from(JSON.stringify({ sub: "attacker@b.com", iat: now, exp: now + 3600 }))
      .toString("base64url");
    const forged = `${header}.${forgedPayload}.${sig}`;
    expect(await verifyJWT(forged, "secret-1")).toBeNull();
  });

  it("rejects garbage input", async () => {
    expect(await verifyJWT("not-a-jwt", "secret-1")).toBeNull();
    expect(await verifyJWT("a.b.c", "secret-1")).toBeNull();
  });
});

describe("code hashing", () => {
  it("hashes deterministically", async () => {
    const a = await hashCode("123456");
    const b = await hashCode("123456");
    expect(a).toBe(b);
  });

  it("hashes different codes to different digests", async () => {
    const a = await hashCode("123456");
    const b = await hashCode("654321");
    expect(a).not.toBe(b);
  });

  it("never stores or compares the plaintext code itself", async () => {
    const hash = await hashCode("000000");
    expect(hash).not.toContain("000000");
  });
});

describe("constantTimeEqual", () => {
  it("reports equal strings as equal", () => {
    expect(constantTimeEqual("abcdef", "abcdef")).toBe(true);
  });

  it("reports different strings as different", () => {
    expect(constantTimeEqual("abcdef", "abcdeg")).toBe(false);
  });

  it("reports different-length strings as different without throwing", () => {
    expect(constantTimeEqual("short", "muchlongerstring")).toBe(false);
  });

  it("catches a difference in the very first character", () => {
    expect(constantTimeEqual("zbcdef", "abcdef")).toBe(false);
  });
});

describe("generateCode", () => {
  it("always produces a zero-padded 6-digit string", () => {
    for (let i = 0; i < 200; i++) {
      const code = generateCode();
      expect(code).toMatch(/^\d{6}$/);
    }
  });
});
