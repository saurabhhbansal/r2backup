import type { Storage, OtpRow, VaultRow, DeviceRow } from "../../src/storage";
import type { RateLimitState } from "../../src/ratelimit";

// An in-memory Storage for tests, so handler logic (including the
// enumeration-resistant response and the rate limiter) can be exercised
// end to end without D1, without Miniflare/workerd, and without a network
// call -- plain Node under Vitest is enough.
export class MemoryStorage implements Storage {
  users = new Map<string, { created_at: number }>();
  otps = new Map<string, OtpRow>();
  vaults = new Map<string, VaultRow>();
  devices = new Map<string, DeviceRow[]>();
  rateLimits = new Map<string, RateLimitState>();

  async upsertUser(email: string, now: number): Promise<void> {
    if (!this.users.has(email)) this.users.set(email, { created_at: now });
  }

  async getOtp(email: string): Promise<OtpRow | null> {
    return this.otps.get(email) ?? null;
  }

  async putOtp(email: string, codeHash: string, expiresAt: number): Promise<void> {
    this.otps.set(email, { code_hash: codeHash, expires_at: expiresAt, attempts: 0 });
  }

  async incrementOtpAttempts(email: string): Promise<number> {
    const row = this.otps.get(email);
    if (!row) return 0;
    row.attempts += 1;
    return row.attempts;
  }

  async deleteOtp(email: string): Promise<void> {
    this.otps.delete(email);
  }

  async getVault(email: string): Promise<VaultRow | null> {
    return this.vaults.get(email) ?? null;
  }

  async putVault(email: string, row: VaultRow): Promise<void> {
    this.vaults.set(email, row);
  }

  async listDevices(email: string): Promise<DeviceRow[]> {
    return [...(this.devices.get(email) ?? [])];
  }

  async upsertDevice(email: string, device: DeviceRow): Promise<void> {
    const existing = this.devices.get(email) ?? [];
    const idx = existing.findIndex((d) => d.device_name === device.device_name);
    if (idx >= 0) existing[idx] = device;
    else existing.push(device);
    this.devices.set(email, existing);
  }

  async getRateLimit(key: string): Promise<RateLimitState | null> {
    return this.rateLimits.get(key) ?? null;
  }

  async setRateLimit(key: string, state: RateLimitState): Promise<void> {
    this.rateLimits.set(key, state);
  }
}

export class FakeMailer {
  sent: Array<{ email: string; code: string; expiresInMinutes: number }> = [];
  failNext = false;

  async sendCode(email: string, code: string, expiresInMinutes: number): Promise<void> {
    if (this.failNext) {
      this.failNext = false;
      throw new Error("simulated send failure");
    }
    this.sent.push({ email, code, expiresInMinutes });
  }
}
