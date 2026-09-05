import { LOGO_PNG_BASE64 } from "../logo";

// The public face of r2backup, such as it is.
//
// This exists for one specific reader. Cloudflare's OAuth consent screen puts
// the client URL next to the verified badge, so the person following it has
// just been asked to grant an unfamiliar program access to their account and
// is checking who is asking. That is not a marketing visit -- it is a "should
// I trust this?" visit, and the page is written to answer that and little
// else: what the program is, exactly what the access is used for, what it
// deliberately cannot do, and where the source is.
//
// It is also the reason the Worker serves anything at / at all. Before this,
// the root returned {"error":"not found"}, so the link beside a verified
// badge led to a 404 in JSON -- which looks precisely like the sort of thing
// nobody should grant account access to.

const CACHE_A_DAY = "public, max-age=86400";

export function handleLanding(): Response {
  return new Response(LANDING_HTML, {
    status: 200,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": CACHE_A_DAY,
      // This page is static, same for everyone, and framed by nobody.
      "x-content-type-options": "nosniff",
      "referrer-policy": "no-referrer",
      "content-security-policy":
        "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
    },
  });
}

export function handleLogo(): Response {
  const binary = atob(LOGO_PNG_BASE64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new Response(bytes, {
    status: 200,
    headers: {
      "content-type": "image/png",
      "cache-control": CACHE_A_DAY,
      "x-content-type-options": "nosniff",
    },
  });
}

const REPO = "https://github.com/saurabhhbansal/r2backup";

const LANDING_HTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>r2backup</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #fbfbfa; --fg: #1b1b1a; --dim: #5b5b57;
    --rule: #e4e4e0; --accent: #0b7285; --card: #ffffff;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #16161a; --fg: #ecebe8; --dim: #a3a29d;
      --rule: #2c2c31; --accent: #4db6c9; --card: #1d1d22;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--fg);
    font: 16px/1.6 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  main { max-width: 42rem; margin: 0 auto; padding: 3rem 1.5rem 4rem; }
  img.logo { width: 260px; max-width: 100%; height: auto; }
  h1 { font-size: 1.1rem; font-weight: 600; margin: 2.5rem 0 .5rem; }
  p { margin: 0 0 1rem; }
  .lede { font-size: 1.15rem; color: var(--fg); margin-top: 1.25rem; }
  .dim { color: var(--dim); }
  hr { border: 0; border-top: 1px solid var(--rule); margin: 2.5rem 0; }
  a { color: var(--accent); }
  ul { margin: 0 0 1rem; padding-left: 1.1rem; }
  li { margin-bottom: .4rem; }
  .card {
    background: var(--card); border: 1px solid var(--rule);
    border-radius: 10px; padding: 1.25rem 1.4rem; margin: 1.25rem 0;
  }
  code {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: .9em; background: var(--card);
    border: 1px solid var(--rule); border-radius: 4px; padding: .1em .35em;
  }
  pre {
    background: var(--card); border: 1px solid var(--rule);
    border-radius: 8px; padding: .9rem 1.1rem; overflow-x: auto;
  }
  pre code { background: none; border: 0; padding: 0; }
  footer { margin-top: 3rem; color: var(--dim); font-size: .9rem; }
</style>
</head>
<body>
<main>
  <img class="logo" src="/logo.png" alt="r2backup">

  <p class="lede">Back up your folders to Cloudflare R2, and get them back anywhere.</p>
  <p class="dim">One static binary. No account required, no background service,
  nothing left running. The operating system's scheduler starts it, it does its
  work, and it exits.</p>

  <hr>

  <h1>If you got here from a Cloudflare sign-in screen</h1>
  <p>You were asked whether r2backup can reach your Cloudflare account. Here is
  exactly what it does with that, and what it cannot do.</p>

  <div class="card">
    <p><strong>What it asks for</strong></p>
    <ul>
      <li><strong>Workers R2 Storage, read and write</strong> — to list the
      buckets on your account so you can pick one, and to create one if you do
      not have a bucket yet.</li>
      <li><strong>Memberships, read</strong> — to see which Cloudflare accounts
      you belong to, so you can choose the right one instead of hunting for an
      account ID.</li>
    </ul>
    <p><strong>What it deliberately does not ask for</strong></p>
    <p class="dim">Cloudflare offers two further scopes that reach
    <em>inside</em> a bucket and could read the files stored there. r2backup
    does not request them. This sign-in manages the container and nothing in
    it; your backed-up files are read and written with the separate R2 keys
    you create yourself, which stay on your own computer.</p>
    <p><strong>How long it lasts</strong></p>
    <p class="dim">Until setup finishes. The sign-in is used once, to find your
    account and your bucket, and is then revoked. Nothing is stored, and there
    is no refresh token to store — scheduled backups run on your R2 keys, not
    on this.</p>
  </div>

  <p class="dim">You can review or withdraw this at any time from
  <strong>Cloudflare dashboard → My Profile → Authorized Applications</strong>.</p>

  <hr>

  <h1>Install</h1>
  <p class="dim">Windows, PowerShell:</p>
  <pre><code>irm ${REPO}/releases/latest/download/install.ps1 | iex</code></pre>
  <p class="dim">macOS and Linux:</p>
  <pre><code>curl -sSL ${REPO}/releases/latest/download/install.sh | sh</code></pre>
  <p class="dim">Both scripts check the download against the published
  checksums. The command is <code>r2b</code>.</p>

  <hr>

  <h1>The source</h1>
  <p>All of it is on GitHub, including this page and the server behind it:
  <a href="${REPO}">${REPO}</a>.</p>

  <footer>
    <p>r2backup is an independent open-source project. It is not affiliated
    with, or endorsed by, Cloudflare.</p>
  </footer>
</main>
</body>
</html>
`;
