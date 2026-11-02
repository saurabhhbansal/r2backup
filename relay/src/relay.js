// r2backup relay: Ultra fast mode's Worker.
//
// It runs in the user's own Cloudflare account, put there by r2backup with
// the user's API token, and does one thing: copy a byte range from one URL to
// another without the bytes passing through the user's computer. r2backup
// decides what to copy and hands it two temporary links per request -- a
// download link from Drime and a presigned S3 upload link -- so this Worker
// holds no storage keys, signs nothing and keeps nothing.
//
// The bytes are piped from the download straight into the upload. No
// JavaScript touches them, which is what keeps a copy of any size inside the
// free plan's 10 ms of CPU per request: waiting on the network is not CPU.
//
// Every request must carry the secret r2backup generated for this Worker
// (RELAY_SECRET), which only the computer that set it up knows.

/** Bumped whenever the protocol or behaviour changes, so r2backup can tell an
 * old relay from the one it carries and upload the new one. */
export const VERSION = "2";

/** Headers the caller may set on the upload. Presigned S3 uploads sign the
 * metadata and content-type headers, so they must be sent exactly. */
const ALLOWED_UPLOAD_HEADER = /^(x-amz-[a-z0-9-]+|content-type|content-md5|cache-control|content-disposition|content-encoding|content-language|expires)$/;

/**
 * @typedef {{ url: string, start?: number, end?: number }} Source
 * @typedef {{ url: string, headers?: Record<string, string> }} Destination
 * @typedef {{ src: Source, dst: Destination, length: number }} CopyRequest
 * @typedef {{ fetch: typeof fetch, FixedLengthStream: any }} Deps
 */

/**
 * @param {number} status
 * @param {unknown} body
 */
function json(status, body) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", "cache-control": "no-store" },
  });
}

/**
 * Compares two strings in time that does not depend on where they differ.
 * @param {string} a
 * @param {string} b
 */
export function sameSecret(a, b) {
  const enc = new TextEncoder();
  const x = enc.encode(a);
  const y = enc.encode(b);
  // Different lengths still walk the longer string, so length is all a
  // timing attack could learn -- and the secret's length is not a secret.
  let diff = x.length ^ y.length;
  const n = Math.max(x.length, y.length);
  for (let i = 0; i < n; i++) {
    diff |= (x[i] ?? 0) ^ (y[i] ?? 0);
  }
  return diff === 0;
}

/** @param {unknown} u */
function httpsURL(u) {
  if (typeof u !== "string") return null;
  try {
    const url = new URL(u);
    return url.protocol === "https:" ? url : null;
  } catch {
    return null;
  }
}

/**
 * Checks a copy request's shape. Returns a reason when it is wrong.
 * @param {any} body
 * @returns {string | null}
 */
export function invalid(body) {
  if (!body || typeof body !== "object") return "the request is not a JSON object";
  const { src, dst, length } = body;
  if (!src || !httpsURL(src.url)) return "src.url must be an https URL";
  if (!dst || !httpsURL(dst.url)) return "dst.url must be an https URL";
  if (!Number.isSafeInteger(length) || length < 1) return "length must be a whole number of bytes, at least 1";
  const ranged = src.start !== undefined || src.end !== undefined;
  if (ranged) {
    if (!Number.isSafeInteger(src.start) || !Number.isSafeInteger(src.end) || src.start < 0 || src.end < src.start) {
      return "src.start and src.end must be a byte range";
    }
    if (src.end - src.start + 1 !== length) return "the range and the length do not agree";
  }
  if (dst.headers !== undefined) {
    if (typeof dst.headers !== "object" || dst.headers === null) return "dst.headers must be an object";
    for (const [k, v] of Object.entries(dst.headers)) {
      if (!ALLOWED_UPLOAD_HEADER.test(k.toLowerCase())) return `dst.headers may not set ${k}`;
      if (typeof v !== "string") return `dst.headers.${k} must be a string`;
    }
  }
  return null;
}

/**
 * Reads at most a little of a failed response, for the error message.
 * @param {Response} resp
 */
async function snippet(resp) {
  try {
    const text = await resp.text();
    return text.slice(0, 300);
  } catch {
    return "";
  }
}

/**
 * Copies one range from src to dst.
 * @param {CopyRequest} req
 * @param {Deps} deps
 */
export async function copy(req, deps) {
  // Called bare, never as deps.fetch(...): the Workers runtime refuses a
  // fetch whose `this` is anything but the global scope ("Illegal
  // invocation"), where Node does not mind.
  const fetchFn = deps.fetch;
  const srcHeaders = new Headers();
  if (req.src.start !== undefined) {
    srcHeaders.set("range", `bytes=${req.src.start}-${req.src.end}`);
  }
  const down = await fetchFn(req.src.url, { headers: srcHeaders, redirect: "follow" });
  const wantStatus = req.src.start !== undefined ? 206 : 200;
  if (down.status !== wantStatus) {
    return json(502, {
      ok: false, stage: "source", status: down.status,
      error: `the download answered ${down.status}, expected ${wantStatus}: ${await snippet(down)}`,
    });
  }
  const got = down.headers.get("content-length");
  if (got !== null && Number(got) !== req.length) {
    await down.body?.cancel();
    return json(502, {
      ok: false, stage: "source", status: down.status,
      error: `the download is ${got} bytes, expected ${req.length}`,
    });
  }
  if (!down.body) {
    return json(502, { ok: false, stage: "source", status: down.status, error: "the download has no body" });
  }

  // S3 needs a Content-Length on an upload; a FixedLengthStream gives the
  // runtime one to send, and fails the upload if the bytes fall short.
  const { readable, writable } = new deps.FixedLengthStream(req.length);
  // An upload that fails before reading everything leaves the pipe waiting
  // for a reader that has gone. giveUp stops it from both ends and does not
  // wait for it.
  const stop = new AbortController();
  const giveUp = () => {
    stop.abort();
    readable.cancel().catch(() => {});
  };
  const piping = down.body.pipeTo(writable, { signal: stop.signal }).then(() => null, (/** @type {unknown} */ e) => e);
  const upHeaders = new Headers(req.dst.headers ?? {});
  let up;
  try {
    up = await fetchFn(req.dst.url, { method: "PUT", body: readable, headers: upHeaders });
  } catch (e) {
    giveUp();
    return json(502, { ok: false, stage: "destination", status: 0, error: `the upload failed: ${String(e)}` });
  }
  if (!up.ok) {
    giveUp();
    return json(502, {
      ok: false, stage: "destination", status: up.status,
      error: `the upload answered ${up.status}: ${await snippet(up)}`,
    });
  }
  const pipeErr = await piping;
  if (pipeErr) {
    return json(502, { ok: false, stage: "source", status: 0, error: `the download broke off: ${String(pipeErr)}` });
  }
  return json(200, { ok: true, etag: up.headers.get("etag") ?? "", bytes: req.length });
}

/**
 * @param {Request} request
 * @param {{ RELAY_SECRET?: string }} env
 * @param {Deps} deps
 */
export async function handle(request, env, deps) {
  const secret = env.RELAY_SECRET ?? "";
  const auth = request.headers.get("authorization") ?? "";
  const given = auth.startsWith("Bearer ") ? auth.slice(7) : "";
  // A relay with no secret set refuses everything rather than serving anyone.
  if (secret.length < 32 || !sameSecret(given, secret)) {
    return json(401, { ok: false, error: "not allowed" });
  }
  const url = new URL(request.url);
  if (request.method === "GET" && url.pathname === "/health") {
    return json(200, { ok: true, version: VERSION });
  }
  if (request.method === "POST" && url.pathname === "/copy") {
    let body;
    try {
      body = await request.json();
    } catch {
      return json(400, { ok: false, error: "the request is not JSON" });
    }
    const why = invalid(body);
    if (why) return json(400, { ok: false, error: why });
    return copy(/** @type {CopyRequest} */ (body), deps);
  }
  return json(404, { ok: false, error: "no such route" });
}

export default {
  /**
   * @param {Request} request
   * @param {{ RELAY_SECRET?: string }} env
   */
  async fetch(request, env) {
    // fetch is wrapped so it is always called on the global scope, which is
    // the only `this` the Workers runtime accepts for it.
    return handle(request, env, {
      fetch: (input, init) => fetch(input, init),
      // @ts-ignore FixedLengthStream is a Workers runtime global.
      FixedLengthStream: globalThis.FixedLengthStream,
    });
  },
};
