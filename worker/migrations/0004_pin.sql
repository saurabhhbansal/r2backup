-- A 6-digit PIN after the email code, and storage keys the server cannot read
-- on its own.
--
-- The client stretches the PIN with Argon2id (salt and parameters below) into
-- two halves. One half goes to the server as proof of the PIN; the server
-- keeps only an HMAC of it under a key derived from KEYS_SECRET, so the
-- database alone is not enough to test PIN guesses. The other half never
-- leaves the computer: with a per-account share the server hands back after a
-- correct PIN, it makes the key that encrypts the storage keys. The server
-- stores that ciphertext in sealed_keys and never sees the key.
--
-- Six digits is a small space. What stops guessing is the lockout here
-- (failures/locked_until) and the fact that an offline search needs both this
-- database and the Worker's secrets.
--
-- version increases every time a PIN is set or reset. Sessions carry the
-- version they were issued under, so resetting the PIN signs every computer
-- out.
CREATE TABLE IF NOT EXISTS pins (
  email TEXT PRIMARY KEY,
  salt TEXT NOT NULL,
  kdf_params TEXT NOT NULL,
  verifier TEXT NOT NULL,
  version INTEGER NOT NULL,
  failures INTEGER NOT NULL DEFAULT 0,
  locked_until INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

-- Storage keys, encrypted by the client. Opaque to the server.
--
-- The server-readable stored_keys table (0003) is emptied for an account the
-- moment it saves keys here; until then, setting a PIN hands its contents back
-- once so the client can re-encrypt them.
CREATE TABLE IF NOT EXISTS sealed_keys (
  email TEXT PRIMARY KEY,
  ciphertext TEXT NOT NULL,
  nonce TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
