// /api/account/* -> the account Worker (r2backup-account), through a service
// binding, so the app's sign-in and key sync work from r2backup.pages.dev
// alone -- the one address a locked-down network has to allow.
//
// The request is passed on as it came, headers included. That keeps
// CF-Connecting-IP, which Cloudflare's edge sets on the way in here and the
// account Worker rate-limits on.
export async function onRequest({ request, env }) {
  if (!env.ACCOUNT) {
    return Response.json({ error: "account service not bound" }, { status: 503 });
  }
  const url = new URL(request.url);
  const rest = url.pathname.slice("/api/account".length) || "/";
  const target = new URL(rest + url.search, "https://r2backup-account.internal");
  return env.ACCOUNT.fetch(new Request(target, request));
}
