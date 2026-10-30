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

- The `r2b` and `r2bw` programs in this repository.
- The account service in [`worker/`](worker): sign-in codes, sessions, and
  the storage keys it keeps for an account.
- The install scripts in [`scripts/`](scripts).

Cloudflare R2 and Drime themselves are out of scope; report problems with
them to those companies.
