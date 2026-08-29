-- Core account tables.
--
-- Every table is keyed by the normalised email address (lowercased,
-- trimmed) rather than a synthetic id, because the email *is* the account
-- identity in a magic-code flow -- there is no separate signup step that
-- would mint one.

CREATE TABLE IF NOT EXISTS users (
  email TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL
);

-- One outstanding code per email at a time: requesting a new one overwrites
-- the row (see `putOtp`), which is what makes "codes are single-use" and
-- "a stale code stops working the moment a fresh one is issued" the same
-- mechanism instead of two.
CREATE TABLE IF NOT EXISTS otps (
  email TEXT PRIMARY KEY,
  code_hash TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0
);

-- ciphertext/nonce/kdf_params are opaque TEXT (base64 / JSON produced by the
-- Go client) on purpose -- the server stores bytes, it never decodes them.
CREATE TABLE IF NOT EXISTS vaults (
  email TEXT PRIMARY KEY,
  ciphertext TEXT NOT NULL,
  nonce TEXT NOT NULL,
  kdf_params TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS devices (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  email TEXT NOT NULL,
  device_name TEXT NOT NULL,
  os TEXT NOT NULL,
  last_seen INTEGER NOT NULL,
  UNIQUE (email, device_name)
);

CREATE INDEX IF NOT EXISTS idx_devices_email ON devices (email);

-- Supporting infrastructure for "rate-limit per email and per IP" -- not
-- part of the account domain model, just where the counters live. Keyed by
-- an arbitrary bucket key ("email:<addr>" or "ip:<addr>") so one table
-- serves both limits.
CREATE TABLE IF NOT EXISTS rate_limits (
  bucket_key TEXT PRIMARY KEY,
  window_start INTEGER NOT NULL,
  count INTEGER NOT NULL
);
