// Worker bindings. JWT_SECRET and RESEND_API_KEY are Worker Secrets (set via
// `wrangler secret put`), never [vars] in wrangler.toml -- see the comment
// there for why.
export interface Env {
  DB: D1Database;
  JWT_SECRET: string;
  RESEND_API_KEY: string;
  // Encrypts the storage keys an account keeps (see handlers/keys.ts).
  // Changing it makes every stored set unreadable; people then enter their
  // keys once more.
  KEYS_SECRET: string;
}
