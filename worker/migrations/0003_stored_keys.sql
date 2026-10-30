-- Storage keys an account keeps for its computers, without a password.
--
-- Signing in is email-code only, so there is no passphrase left to derive a
-- client-side key from. The keys are encrypted here instead, with AES-GCM
-- under a key derived from the KEYS_SECRET Worker secret, and the account's
-- email bound in as additional data so a row copied to another account does
-- not open. Anyone holding KEYS_SECRET and the database can read them; that
-- is the trade made for "type your keys once, sign in anywhere with a code".
--
-- The old password-locked `vaults` table stays: clients from before this
-- change still read and write it.
CREATE TABLE IF NOT EXISTS stored_keys (
  email TEXT PRIMARY KEY,
  ciphertext TEXT NOT NULL,
  iv TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
