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
    const ctx: AppContext = { storage, mailer: new FakeMailer(), jwtSecret: SECRET, now: () => 42 };

    const postRes = await handlePostDevice(
      await authedRequest("POST", "carol@example.com", { device_name: "Carol's Laptop", os: "windows" }),
      ctx,
    );
    expect(postRes.status).toBe(200);

    const listRes = await handleGetDevices(await authedRequest("GET", "carol@example.com"), ctx);
    const { devices } = (await listRes.json()) as { devices: Array<{ device_name: string; os: string; last_seen: number }> };
    expect(devices).toEqual([{ device_name: "Carol's Laptop", os: "windows", last_seen: 42 }]);
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
