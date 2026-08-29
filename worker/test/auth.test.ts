import { describe, it, expect, beforeEach } from "vitest";
import { handleRequestCode, handleVerify } from "../src/handlers/auth";
import { verifyJWT } from "../src/crypto";
import { MemoryStorage, FakeMailer } from "./support/memoryStorage";
import type { AppContext } from "../src/context";

function makeContext(storage: MemoryStorage, mailer: FakeMailer, clock: { now: number }): AppContext {
  return {
    storage,
    mailer,
    jwtSecret: "test-secret",
    now: () => clock.now,
  };
}

function postJson(path: string, body: unknown): Request {
  return new Request(`https://r2backup.flexpod.cc${path}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

describe("POST /auth/request — enumeration resistance", () => {
  let storage: MemoryStorage;
  let mailer: FakeMailer;
  let clock: { now: number };
  let ctx: AppContext;

  beforeEach(() => {
    storage = new MemoryStorage();
    mailer = new FakeMailer();
    clock = { now: 1_000_000 };
    ctx = makeContext(storage, mailer, clock);
  });

  it("returns an identical response for an unknown email and an existing account", async () => {
    await storage.upsertUser("known@example.com", clock.now);

    const resUnknown = await handleRequestCode(postJson("/auth/request", { email: "unknown@example.com" }), ctx, "1.1.1.1");
    const resKnown = await handleRequestCode(postJson("/auth/request", { email: "known@example.com" }), ctx, "2.2.2.2");

    expect(resUnknown.status).toBe(resKnown.status);
    expect(await resUnknown.clone().json()).toEqual(await resKnown.clone().json());
    expect(resUnknown.status).toBe(200);
  });

  it("still sends a code and creates an OTP row for an email with no account", async () => {
    await handleRequestCode(postJson("/auth/request", { email: "brand-new@example.com" }), ctx, "3.3.3.3");
    expect(mailer.sent).toHaveLength(1);
    expect(mailer.sent[0].email).toBe("brand-new@example.com");
    expect(await storage.getOtp("brand-new@example.com")).not.toBeNull();
  });

  it("normalises the email before using it as a key", async () => {
    await handleRequestCode(postJson("/auth/request", { email: "  MiXed@Example.COM " }), ctx, "4.4.4.4");
    expect(await storage.getOtp("mixed@example.com")).not.toBeNull();
    expect(mailer.sent[0].email).toBe("mixed@example.com");
  });

  it("rejects a malformed body without touching storage", async () => {
    const res = await handleRequestCode(postJson("/auth/request", { email: 42 }), ctx, "5.5.5.5");
    expect(res.status).toBe(400);
    expect(mailer.sent).toHaveLength(0);
  });

  it("still returns the generic response even when sending the email fails", async () => {
    mailer.failNext = true;
    const res = await handleRequestCode(postJson("/auth/request", { email: "unlucky@example.com" }), ctx, "6.6.6.6");
    expect(res.status).toBe(200);
    expect(await res.clone().json()).toEqual({ ok: true });
  });

  it("rate-limits repeated requests for the same email", async () => {
    const email = "hammered@example.com";
    let last;
    for (let i = 0; i < 4; i++) {
      last = await handleRequestCode(postJson("/auth/request", { email }), ctx, `10.0.0.${i}`);
    }
    expect(last?.status).toBe(429);
  });

  it("rate-limits repeated requests from the same IP across different emails", async () => {
    let last;
    for (let i = 0; i < 11; i++) {
      last = await handleRequestCode(postJson("/auth/request", { email: `user${i}@example.com` }), ctx, "9.9.9.9");
    }
    expect(last?.status).toBe(429);
  });
});

describe("POST /auth/verify", () => {
  let storage: MemoryStorage;
  let mailer: FakeMailer;
  let clock: { now: number };
  let ctx: AppContext;

  beforeEach(() => {
    storage = new MemoryStorage();
    mailer = new FakeMailer();
    // Real wall-clock time, not an arbitrary epoch: verifyJWT checks `exp`
    // against the actual Date.now() (JWT expiry is a real-world concept,
    // not something that should follow the app's injectable clock), so a
    // token minted against a fake "now" of 1970 would already look expired.
    clock = { now: Math.floor(Date.now() / 1000) };
    ctx = makeContext(storage, mailer, clock);
  });

  async function requestAndGetCode(email: string): Promise<string> {
    await handleRequestCode(postJson("/auth/request", { email }), ctx, "1.1.1.1");
    const sent = mailer.sent[mailer.sent.length - 1];
    if (!sent) throw new Error("no code sent");
    return sent.code;
  }

  it("issues a 30-day JWT for the correct code", async () => {
    const email = "verify-me@example.com";
    const code = await requestAndGetCode(email);

    const res = await handleVerify(postJson("/auth/verify", { email, code }), ctx);
    expect(res.status).toBe(200);
    const { token } = (await res.json()) as { token: string };

    const payload = await verifyJWT(token, "test-secret");
    expect(payload).not.toBeNull();
    expect(payload?.sub).toBe(email);
    expect(payload!.exp - payload!.iat).toBe(30 * 24 * 60 * 60);
  });

  it("rejects a wrong code without consuming the real one", async () => {
    const email = "wrong-code@example.com";
    const code = await requestAndGetCode(email);

    const bad = await handleVerify(postJson("/auth/verify", { email, code: "000000" === code ? "111111" : "000000" }), ctx);
    expect(bad.status).toBe(400);

    const good = await handleVerify(postJson("/auth/verify", { email, code }), ctx);
    expect(good.status).toBe(200);
  });

  it("kills the code after 5 wrong attempts, even with the right code left", async () => {
    const email = "five-strikes@example.com";
    const code = await requestAndGetCode(email);
    const wrongCode = code === "000000" ? "111111" : "000000";

    for (let i = 0; i < 5; i++) {
      await handleVerify(postJson("/auth/verify", { email, code: wrongCode }), ctx);
    }

    const res = await handleVerify(postJson("/auth/verify", { email, code }), ctx);
    expect(res.status).toBe(400);
  });

  it("rejects an expired code", async () => {
    const email = "expired@example.com";
    const code = await requestAndGetCode(email);

    clock.now += 11 * 60; // past the 10-minute TTL

    const res = await handleVerify(postJson("/auth/verify", { email, code }), ctx);
    expect(res.status).toBe(400);
  });

  it("is single-use: the same code cannot verify twice", async () => {
    const email = "single-use@example.com";
    const code = await requestAndGetCode(email);

    const first = await handleVerify(postJson("/auth/verify", { email, code }), ctx);
    const second = await handleVerify(postJson("/auth/verify", { email, code }), ctx);

    expect(first.status).toBe(200);
    expect(second.status).toBe(400);
  });

  it("rejects verifying an email that never requested a code", async () => {
    const res = await handleVerify(postJson("/auth/verify", { email: "never-asked@example.com", code: "123456" }), ctx);
    expect(res.status).toBe(400);
  });
});
