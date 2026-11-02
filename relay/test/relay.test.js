import { describe, it, expect } from "vitest";
import worker, { handle, invalid, sameSecret, VERSION } from "../src/relay.js";

const SECRET = "s".repeat(48);
const env = { RELAY_SECRET: SECRET };

// FixedLengthStream is a Workers global. This stand-in behaves the same way
// for what the relay relies on: it passes bytes through and errors when the
// count is not exactly the length it was given.
class FixedLengthStream extends TransformStream {
  /** @param {number} length */
  constructor(length) {
    let seen = 0;
    super({
      transform(chunk, ctl) {
        seen += chunk.byteLength;
        if (seen > length) {
          ctl.error(new TypeError("too many bytes"));
          return;
        }
        ctl.enqueue(chunk);
      },
      flush(ctl) {
        if (seen !== length) ctl.error(new TypeError(`expected ${length} bytes, got ${seen}`));
      },
    });
    this.expected = length;
  }
}

/** @param {Uint8Array} data */
function streamOf(data, { breakAfter = -1 } = {}) {
  return new ReadableStream({
    start(ctl) {
      if (breakAfter >= 0) {
        ctl.enqueue(data.slice(0, breakAfter));
        ctl.error(new Error("connection reset"));
        return;
      }
      // Several chunks, as a real download arrives.
      for (let i = 0; i < data.length; i += 3) ctl.enqueue(data.slice(i, i + 3));
      ctl.close();
    },
  });
}

/**
 * A fake network: one source URL serving data (with Range), and one upload
 * URL that records what it was sent.
 */
function network({ data = new TextEncoder().encode("hello, cloud to cloud"), source = {}, upload = {} } = {}) {
  const calls = { source: [], upload: [] };
  // Like the Workers runtime, this fetch refuses to be called as a method of
  // anything ("Illegal invocation"): called bare, `this` is undefined here
  // (modules are strict) or the global scope. Node's own fetch does not
  // check, which is how deps.fetch(...) once passed these tests and threw on
  // every copy in a real Worker.
  /** @type {typeof fetch} */
  const fakeFetch = async function (input, init = {}) {
    if (this !== undefined && this !== globalThis) {
      throw new TypeError("Illegal invocation: function called with incorrect `this` reference.");
    }
    const url = String(input);
    if (url.startsWith("https://drime.example/")) {
      const headers = new Headers(init.headers);
      calls.source.push({ url, range: headers.get("range") });
      if (source.status) return new Response("nope", { status: source.status });
      let body = data;
      let status = 200;
      const range = headers.get("range");
      if (range) {
        const [a, b] = range.replace("bytes=", "").split("-").map(Number);
        body = data.slice(a, b + 1);
        status = 206;
      }
      const len = source.length ?? body.length;
      return new Response(streamOf(body, source), { status, headers: { "content-length": String(len) } });
    }
    if (url.startsWith("https://s3.example/")) {
      const headers = new Headers(init.headers);
      const rec = { url, method: init.method, headers: Object.fromEntries(headers), body: null };
      calls.upload.push(rec);
      if (upload.throws) throw new Error("network down");
      if (upload.status) {
        // Answers without reading the body, as S3 does when a signature
        // is wrong. The relay must not wait forever for it to be read.
        return new Response("<Error>SignatureDoesNotMatch</Error>", { status: upload.status });
      }
      rec.body = new Uint8Array(await new Response(init.body).arrayBuffer());
      return new Response(null, { status: 200, headers: { etag: '"abc123"' } });
    }
    throw new Error("unexpected fetch " + url);
  };
  return { calls, deps: { fetch: fakeFetch, FixedLengthStream } };
}

function req(path, { method = "POST", body, secret = SECRET } = {}) {
  const headers = { "content-type": "application/json" };
  if (secret !== null) headers.authorization = "Bearer " + secret;
  return new Request("https://relay.example.workers.dev" + path, {
    method,
    headers,
    body: body === undefined ? undefined : typeof body === "string" ? body : JSON.stringify(body),
  });
}

const copyBody = (over = {}) => ({
  src: { url: "https://drime.example/file?sig=1", start: 7, end: 11 },
  dst: { url: "https://s3.example/bucket/key?X-Amz-Signature=x", headers: { "x-amz-meta-mtime": "2026-01-01T00:00:00Z" } },
  length: 5,
  ...over,
});

describe("who may use the relay", () => {
  it("refuses a request with no secret", async () => {
    const { deps } = network();
    const r = await handle(req("/health", { method: "GET", secret: null }), env, deps);
    expect(r.status).toBe(401);
  });

  it("refuses a wrong secret", async () => {
    const { deps } = network();
    const r = await handle(req("/health", { method: "GET", secret: "x".repeat(48) }), env, deps);
    expect(r.status).toBe(401);
  });

  it("refuses everyone when no secret was set up", async () => {
    const { deps } = network();
    const r = await handle(req("/health", { method: "GET", secret: "" }), {}, deps);
    expect(r.status).toBe(401);
  });

  it("refuses everyone when the set secret is too short to be one r2backup made", async () => {
    const { deps } = network();
    const r = await handle(req("/health", { method: "GET", secret: "short" }), { RELAY_SECRET: "short" }, deps);
    expect(r.status).toBe(401);
  });

  it("answers health with its version", async () => {
    const { deps } = network();
    const r = await handle(req("/health", { method: "GET" }), env, deps);
    expect(r.status).toBe(200);
    expect(await r.json()).toEqual({ ok: true, version: VERSION });
  });

  it("compares secrets exactly", () => {
    expect(sameSecret("abc", "abc")).toBe(true);
    expect(sameSecret("abc", "abd")).toBe(false);
    expect(sameSecret("abc", "abcd")).toBe(false);
    expect(sameSecret("", "")).toBe(true);
  });
});

describe("checking a copy request", () => {
  it("accepts a ranged request", () => {
    expect(invalid(copyBody())).toBeNull();
  });
  it("accepts a whole-file request", () => {
    expect(invalid(copyBody({ src: { url: "https://drime.example/f" } }))).toBeNull();
  });
  it("wants https on both ends", () => {
    expect(invalid(copyBody({ src: { url: "http://drime.example/f" } }))).toMatch(/src.url/);
    expect(invalid(copyBody({ dst: { url: "file:///etc/passwd" } }))).toMatch(/dst.url/);
  });
  it("wants the range and length to agree", () => {
    expect(invalid(copyBody({ length: 6 }))).toMatch(/agree/);
    expect(invalid(copyBody({ src: { url: "https://drime.example/f", start: 5, end: 2 } }))).toMatch(/range/);
  });
  it("wants a length", () => {
    expect(invalid(copyBody({ length: 0 }))).toMatch(/length/);
    expect(invalid(copyBody({ length: 1.5 }))).toMatch(/length/);
  });
  it("does not let the caller set any header it likes on the upload", () => {
    expect(invalid(copyBody({ dst: { url: "https://s3.example/k", headers: { host: "evil" } } }))).toMatch(/may not set host/);
    expect(invalid(copyBody({ dst: { url: "https://s3.example/k", headers: { authorization: "x" } } }))).toMatch(/authorization/);
  });
  it("answers 400 for a bad request and 404 for an unknown route", async () => {
    const { deps } = network();
    expect((await handle(req("/copy", { body: "not json" }), env, deps)).status).toBe(400);
    expect((await handle(req("/copy", { body: { src: {} } }), env, deps)).status).toBe(400);
    expect((await handle(req("/nowhere", { body: {} }), env, deps)).status).toBe(404);
  });
});

describe("copying", () => {
  it("streams the asked-for range into the upload with the signed headers", async () => {
    const { deps, calls } = network();
    const r = await handle(req("/copy", { body: copyBody() }), env, deps);
    expect(r.status).toBe(200);
    expect(await r.json()).toEqual({ ok: true, etag: '"abc123"', bytes: 5 });
    expect(calls.source[0].range).toBe("bytes=7-11");
    const up = calls.upload[0];
    expect(up.method).toBe("PUT");
    expect(up.headers["x-amz-meta-mtime"]).toBe("2026-01-01T00:00:00Z");
    expect(new TextDecoder().decode(up.body)).toBe("cloud");
  });

  it("copies a whole file when no range is given", async () => {
    const { deps, calls } = network();
    const body = copyBody({ src: { url: "https://drime.example/f" }, length: 21 });
    const r = await handle(req("/copy", { body }), env, deps);
    expect(r.status).toBe(200);
    expect(calls.source[0].range).toBeNull();
    expect(new TextDecoder().decode(calls.upload[0].body)).toBe("hello, cloud to cloud");
  });

  it("says when the download fails, and uploads nothing", async () => {
    const { deps, calls } = network({ source: { status: 403 } });
    const r = await handle(req("/copy", { body: copyBody() }), env, deps);
    expect(r.status).toBe(502);
    const out = await r.json();
    expect(out).toMatchObject({ ok: false, stage: "source", status: 403 });
    expect(calls.upload).toHaveLength(0);
  });

  it("refuses a download of the wrong size before uploading", async () => {
    const { deps, calls } = network({ source: { length: 99 } });
    const r = await handle(req("/copy", { body: copyBody() }), env, deps);
    expect(r.status).toBe(502);
    expect((await r.json()).error).toMatch(/99 bytes/);
    expect(calls.upload).toHaveLength(0);
  });

  it("says when the upload is refused, without hanging on the unread download", async () => {
    const { deps } = network({ upload: { status: 403 } });
    const r = await handle(req("/copy", { body: copyBody() }), env, deps);
    expect(r.status).toBe(502);
    expect(await r.json()).toMatchObject({ ok: false, stage: "destination", status: 403 });
  });

  it("says when the upload cannot be sent", async () => {
    const { deps } = network({ upload: { throws: true } });
    const r = await handle(req("/copy", { body: copyBody() }), env, deps);
    expect(r.status).toBe(502);
    expect(await r.json()).toMatchObject({ ok: false, stage: "destination" });
  });

  it("fails rather than store a short file when the download breaks off", async () => {
    const { deps } = network({ source: { breakAfter: 2 } });
    const r = await handle(req("/copy", { body: copyBody() }), env, deps);
    expect(r.status).toBe(502);
    expect((await r.json()).ok).toBe(false);
  });
});

describe("the Worker's entry point", () => {
  it("copies through the runtime's own globals, calling fetch as the runtime requires", async () => {
    const { deps, calls } = network();
    const realFetch = globalThis.fetch;
    const realFLS = globalThis.FixedLengthStream;
    globalThis.fetch = deps.fetch;
    globalThis.FixedLengthStream = FixedLengthStream;
    try {
      const r = await worker.fetch(req("/copy", { body: copyBody() }), env);
      expect(await r.json()).toEqual({ ok: true, etag: '"abc123"', bytes: 5 });
      expect(new TextDecoder().decode(calls.upload[0].body)).toBe("cloud");
    } finally {
      globalThis.fetch = realFetch;
      if (realFLS === undefined) delete globalThis.FixedLengthStream;
      else globalThis.FixedLengthStream = realFLS;
    }
  });

  it("the strict fetch really refuses a method call", async () => {
    const { deps } = network();
    await expect(deps.fetch("https://drime.example/f")).rejects.toThrow(/Illegal invocation/);
  });
});
