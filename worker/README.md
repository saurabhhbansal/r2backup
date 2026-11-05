# Account service

The Cloudflare Worker behind r2backup's optional account. It handles email sign-in codes, a PIN, devices, and a small encrypted vault of the storage keys an account keeps, so a second computer can sign in and get its keys without retyping them. The Go client is `internal/account` and `internal/cli/account.go`.

- Code: `src/` (handlers in `src/handlers/`: auth, pin, devices, keys, vault, site, health). TypeScript, no framework.
- Storage: a Cloudflare D1 database (binding `DB`, schema in `migrations/`).
- Config: `wrangler.toml`. It routes the Worker on its workers.dev address and a custom domain.
- Secrets: `JWT_SECRET`, `RESEND_API_KEY` and `KEYS_SECRET` are Worker Secrets, set with `wrangler secret put <NAME>`. They are deliberately not in `wrangler.toml` or anywhere in git. Changing `KEYS_SECRET` makes every stored set of keys unreadable.

## Test

```
cd worker
npm ci
npm test            # vitest, in plain Node against an in-memory Storage; no network
npm run typecheck
```

CI does not run these yet; run them before a pull request that touches `worker/`.

## Run and deploy

```
npm run dev         # wrangler dev
npm run deploy      # wrangler deploy
```

Deploys go to the maintainer's Cloudflare account (the account id and database id are in `wrangler.toml`), so only the maintainer deploys. To try your own copy, create a D1 database, replace those ids in `wrangler.toml`, apply the migrations with `wrangler d1 migrations apply`, and set the secrets above. Do not point a build of `r2b` at a Worker whose secrets you do not control with real keys.

Security issues: see [SECURITY.md](../SECURITY.md).
