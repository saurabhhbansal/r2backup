import type { RateLimitState, RateLimitStore } from "./ratelimit";

export interface OtpRow {
  code_hash: string;
  expires_at: number;
  attempts: number;
}

export interface VaultRow {
  ciphertext: string;
  nonce: string;
  kdf_params: string;
  updated_at: number;
}

export interface DeviceRow {
  device_name: string;
  os: string;
  last_seen: number;
}

// Everything a handler needs from D1, named after the domain operation
// rather than the SQL, so a handler reads like the flow it implements and
// so tests can swap in an in-memory fake without spinning up a database.
export interface Storage extends RateLimitStore {
  upsertUser(email: string, now: number): Promise<void>;
  getOtp(email: string): Promise<OtpRow | null>;
  putOtp(email: string, codeHash: string, expiresAt: number): Promise<void>;
  incrementOtpAttempts(email: string): Promise<number>;
  deleteOtp(email: string): Promise<void>;
  getVault(email: string): Promise<VaultRow | null>;
  putVault(email: string, row: VaultRow): Promise<void>;
  listDevices(email: string): Promise<DeviceRow[]>;
  upsertDevice(email: string, device: DeviceRow): Promise<void>;
}

export class D1Storage implements Storage {
  constructor(private readonly db: D1Database) {}

  async upsertUser(email: string, now: number): Promise<void> {
    await this.db
      .prepare("INSERT INTO users (email, created_at) VALUES (?, ?) ON CONFLICT(email) DO NOTHING")
      .bind(email, now)
      .run();
  }

  async getOtp(email: string): Promise<OtpRow | null> {
    const row = await this.db
      .prepare("SELECT code_hash, expires_at, attempts FROM otps WHERE email = ?")
      .bind(email)
      .first<OtpRow>();
    return row ?? null;
  }

  async putOtp(email: string, codeHash: string, expiresAt: number): Promise<void> {
    // A fresh request replaces any code already outstanding for this email
    // (attempts resets to 0), so requesting a new code is also how an old,
    // possibly-guessed-at code gets retired.
    await this.db
      .prepare(
        `INSERT INTO otps (email, code_hash, expires_at, attempts) VALUES (?, ?, ?, 0)
         ON CONFLICT(email) DO UPDATE SET code_hash = excluded.code_hash, expires_at = excluded.expires_at, attempts = 0`,
      )
      .bind(email, codeHash, expiresAt)
      .run();
  }

  async incrementOtpAttempts(email: string): Promise<number> {
    const row = await this.db
      .prepare("UPDATE otps SET attempts = attempts + 1 WHERE email = ? RETURNING attempts")
      .bind(email)
      .first<{ attempts: number }>();
    return row?.attempts ?? 0;
  }

  async deleteOtp(email: string): Promise<void> {
    await this.db.prepare("DELETE FROM otps WHERE email = ?").bind(email).run();
  }

  async getVault(email: string): Promise<VaultRow | null> {
    const row = await this.db
      .prepare("SELECT ciphertext, nonce, kdf_params, updated_at FROM vaults WHERE email = ?")
      .bind(email)
      .first<VaultRow>();
    return row ?? null;
  }

  async putVault(email: string, row: VaultRow): Promise<void> {
    await this.db
      .prepare(
        `INSERT INTO vaults (email, ciphertext, nonce, kdf_params, updated_at) VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(email) DO UPDATE SET ciphertext = excluded.ciphertext, nonce = excluded.nonce,
           kdf_params = excluded.kdf_params, updated_at = excluded.updated_at`,
      )
      .bind(email, row.ciphertext, row.nonce, row.kdf_params, row.updated_at)
      .run();
  }

  async listDevices(email: string): Promise<DeviceRow[]> {
    const result = await this.db
      .prepare("SELECT device_name, os, last_seen FROM devices WHERE email = ? ORDER BY last_seen DESC")
      .bind(email)
      .all<DeviceRow>();
    return result.results ?? [];
  }

  async upsertDevice(email: string, device: DeviceRow): Promise<void> {
    await this.db
      .prepare(
        `INSERT INTO devices (email, device_name, os, last_seen) VALUES (?, ?, ?, ?)
         ON CONFLICT(email, device_name) DO UPDATE SET os = excluded.os, last_seen = excluded.last_seen`,
      )
      .bind(email, device.device_name, device.os, device.last_seen)
      .run();
  }

  async getRateLimit(key: string): Promise<RateLimitState | null> {
    const row = await this.db
      .prepare("SELECT window_start as windowStart, count FROM rate_limits WHERE bucket_key = ?")
      .bind(key)
      .first<RateLimitState>();
    return row ?? null;
  }

  async setRateLimit(key: string, state: RateLimitState): Promise<void> {
    await this.db
      .prepare(
        `INSERT INTO rate_limits (bucket_key, window_start, count) VALUES (?, ?, ?)
         ON CONFLICT(bucket_key) DO UPDATE SET window_start = excluded.window_start, count = excluded.count`,
      )
      .bind(key, state.windowStart, state.count)
      .run();
  }
}
