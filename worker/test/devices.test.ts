import { describe, it, expect } from "vitest";
import { handlePostDevice, handleGetDevices } from "../src/handlers/devices";
import { signJWT } from "../src/crypto";
import { MemoryStorage, FakeMailer } from "./support/memoryStorage";
import type { AppContext } from "../src/context";

const SECRET = "device-test-secret";

async function authedRequest(method: string, email: string, body?: unknown): Promise<Request> {
  const token = await signJWT({ sub: email, iat: 0, exp: 9_999_999_999 }, SECRET);
  return new Request("https://r2backup.flexpod.cc/devices", {
    method,
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

describe("devices", () => {
  it("registers a device and lists it back", async () => {
    const storage = new MemoryStorage();
    // Mirrors index.ts's real ctx.now(): Unix SECONDS, not milliseconds. A
    // fixture of 42 would round-trip fine no matter which unit either side
    // used, so it could never have caught the Go side decoding this field
    // with time.UnixMilli (every device showing 1 Jan 1970) -- see H2.
    const nowSeconds = () => Math.floor(Date.now() / 1000);
    const ctx: AppContext = { storage, mailer: new FakeMailer(), jwtSecret: SECRET, now: nowSeconds };

    const postRes = await handlePostDevice(
      await authedRequest("POST", "carol@example.com", { device_name: "Carol's Laptop", os: "windows" }),
      ctx,
    );
    expect(postRes.status).toBe(200);

    const listRes = await handleGetDevices(await authedRequest("GET", "carol@example.com"), ctx);
    const { devices } = (await listRes.json()) as { devices: Array<{ device_name: string; os: string; last_seen: number }> };
    expect(devices).toHaveLength(1);
    expect(devices[0].device_name).toBe("Carol's Laptop");
    expect(devices[0].os).toBe("windows");
    // Pin the unit: last_seen must land within a couple of seconds of
    // Date.now()/1000. If the worker ever switched to milliseconds this
    // would be off by a factor of ~1000 and fail immediately.
    expect(devices[0].last_seen).toBeGreaterThanOrEqual(nowSeconds() - 2);
    expect(devices[0].last_seen).toBeLessThanOrEqual(nowSeconds() + 2);
  });

  it("re-registering the same device name updates it instead of duplicating", async () => {
    const storage = new MemoryStorage();
    let now = 1;
    const ctx: AppContext = { storage, mailer: new FakeMailer(), jwtSecret: SECRET, now: () => now };

    await handlePostDevice(await authedRequest("POST", "dave@example.com", { device_name: "Desktop", os: "linux" }), ctx);
    now = 2;
    await handlePostDevice(await authedRequest("POST", "dave@example.com", { device_name: "Desktop", os: "linux" }), ctx);

    const listRes = await handleGetDevices(await authedRequest("GET", "dave@example.com"), ctx);
    const { devices } = (await listRes.json()) as { devices: unknown[] };
    expect(devices).toHaveLength(1);
  });

  it("requires authentication", async () => {
    const storage = new MemoryStorage();
    const ctx: AppContext = { storage, mailer: new FakeMailer(), jwtSecret: SECRET, now: () => 1 };
    const res = await handleGetDevices(new Request("https://r2backup.flexpod.cc/devices"), ctx);
    expect(res.status).toBe(401);
  });
});
