import type { Storage } from "./storage";
import type { Mailer } from "./email";

// Everything a handler depends on, gathered in one place and built once per
// request in index.ts. Handlers take this instead of `Env` directly so a
// test can hand them an in-memory Storage and a Mailer that just records
// what it was asked to send, and exercise the real handler code with no D1
// binding, no workerd, and no network call to Resend.
export interface AppContext {
  storage: Storage;
  mailer: Mailer;
  jwtSecret: string;
  now: () => number; // seconds since epoch; a fixed function in tests
}
