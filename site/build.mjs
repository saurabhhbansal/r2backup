// Builds site/public for https://r2backup.pages.dev.
//
//   node build.mjs            newest GitHub release
//   node build.mjs v1.6.1     a given release
//
// It copies three.js and the install scripts in, downloads the release's
// assets into public/download/<tag>/, and writes:
//   public/api/release/latest.json  what `r2b update` and the panel read
//                                   (same field names as GitHub's API)
//   public/_redirects               /download/latest/* -> /download/<tag>/*
//
// Needs the GitHub CLI (gh), signed in. The machine running this can reach
// GitHub; the people downloading from the site do not need to.
import { execFileSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const pub = join(here, "public");
const repoRoot = join(here, "..");
const SITE = (process.env.R2BACKUP_SITE || "https://r2backup.pages.dev").replace(/\/$/, "");
const REPO = "saurabhhbansal/r2backup";

const gh = (...args) => execFileSync("gh", args, { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] });

// three.js, served from this site rather than a CDN a filtered network may block.
mkdirSync(join(pub, "vendor"), { recursive: true });
cpSync(join(here, "node_modules/three/build/three.module.min.js"), join(pub, "vendor/three.module.min.js"));

// Geist and Geist Mono (SIL OFL), served from this site for the same reason.
mkdirSync(join(pub, "fonts"), { recursive: true });
for (const [pkg, face] of [["geist", "geist"], ["geist-mono", "geist-mono"]]) {
  for (const subset of ["latin", "latin-ext"]) {
    const file = `${face}-${subset}-wght-normal.woff2`;
    cpSync(join(here, `node_modules/@fontsource-variable/${pkg}/files/${file}`), join(pub, "fonts", file));
  }
}

// The install scripts, at short addresses: irm https://r2backup.pages.dev/install.ps1 | iex
cpSync(join(repoRoot, "scripts/install.ps1"), join(pub, "install.ps1"));
cpSync(join(repoRoot, "scripts/install.sh"), join(pub, "install.sh"));

const tag = process.argv[2] || gh("release", "view", "--repo", REPO, "--json", "tagName", "-q", ".tagName").trim();
const rel = JSON.parse(gh("release", "view", tag, "--repo", REPO, "--json", "tagName,body,publishedAt,assets"));

// Only the newest release is kept on the site: every installed copy updates
// to the newest one, and Pages counts files.
const dlRoot = join(pub, "download");
mkdirSync(dlRoot, { recursive: true });
for (const d of readdirSync(dlRoot)) if (d !== tag) rmSync(join(dlRoot, d), { recursive: true, force: true });
const dir = join(dlRoot, tag);
mkdirSync(dir, { recursive: true });

const missing = rel.assets.filter((a) => {
  const f = join(dir, a.name);
  return !existsSync(f) || statSync(f).size !== a.size;
});
if (missing.length) {
  console.log(`Downloading ${missing.length} asset(s) of ${tag}...`);
  for (const a of missing) gh("release", "download", tag, "--repo", REPO, "-D", dir, "-p", a.name, "--clobber");
}
for (const a of rel.assets) {
  const size = statSync(join(dir, a.name)).size;
  if (size !== a.size) throw new Error(`${a.name}: downloaded ${size} bytes, release says ${a.size}`);
  if (size > 25 * 1024 * 1024) throw new Error(`${a.name} is over Cloudflare Pages' 25 MiB file limit`);
}

const listing = {
  tag_name: rel.tagName,
  published_at: rel.publishedAt,
  body: rel.body || "",
  assets: rel.assets.map((a) => ({
    name: a.name,
    size: a.size,
    browser_download_url: `${SITE}/download/${rel.tagName}/${encodeURIComponent(a.name)}`,
  })),
};
mkdirSync(join(pub, "api/release"), { recursive: true });
writeFileSync(join(pub, "api/release/latest.json"), JSON.stringify(listing, null, 2) + "\n");
writeFileSync(join(pub, "_redirects"), `/download/latest/* /download/${rel.tagName}/:splat 302\n`);

console.log(`Built site for ${rel.tagName}: ${rel.assets.length} downloads.`);
