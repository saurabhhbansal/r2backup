# Security

r2backup handles storage keys and, through them, everything you back up, so
security reports are taken seriously.

## Reporting a problem

Please report vulnerabilities privately through GitHub: on this repository,
open **Security → Report a vulnerability**. Do not open a public issue for
anything that could put someone's keys or files at risk.

Include what you found, how to reproduce it, and what an attacker could do
with it. You should hear back within a week.

## Supported versions

Only the latest release is supported. `r2b update` (or **Check for
updates** in the taskbar app) moves you to it.

## What is in scope

- The `r2b` and `r2bw` programs in this repository, including how they keep and send storage keys and Drime tokens.
- The account service in [`worker/`](worker): sign-in codes, sessions, and the storage keys it keeps for an account.
- The Ultra fast mode relay in [`relay/`](relay): the Worker r2backup uploads to a person's own Cloudflare account, and how r2backup sets it up and protects it.
- The install scripts in [`scripts/`](scripts).

Drime, Cloudflare (including R2), Backblaze B2 and other S3 providers are out of scope; report problems with them to those companies. If r2backup uses one of them in a way that exposes keys or files, that is in scope.
