#!/usr/bin/env bash
# Mirror a GitHub release on https://r2backup.pages.dev and redeploy the site.
#
#   scripts/publish_site.sh           newest release
#   scripts/publish_site.sh v1.6.1    a given release
#
# Needs gh (signed in) and wrangler credentials for the Cloudflare account
# (`wrangler login`, or CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID in CI).
# The release workflow runs this after every tag once CLOUDFLARE_API_TOKEN is set.
set -euo pipefail
cd "$(dirname "$0")/../site"
npm ci --no-audit --no-fund
node build.mjs "${1:-}"
npx wrangler pages deploy --branch main --commit-dirty=true
