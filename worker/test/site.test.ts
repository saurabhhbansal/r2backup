import { describe, it, expect } from "vitest";
import worker from "../src/index";
import { handleLanding, handleLogo } from "../src/handlers/site";
import type { Env } from "../src/types";

// The public pages read nothing from the environment -- they are the same
// bytes for everybody -- so a stub is enough to drive the real router and
// prove these paths are reachable before buildContext needs anything real.
const stubEnv = {
  DB: null,
  JWT_SECRET: "unused",
  RESEND_API_KEY: "unused",
} as unknown as Env;

function get(path: string, method = "GET"): Promise<Response> {
  return worker.fetch(
    new Request("https://r2backup.flexpod.cc" + path, { method }),
    stubEnv,
  );
}

describe("the public pages", () => {
  // Why these routes exist at all: before them the client URL that Cloudflare
  // puts beside a verified badge on its consent screen returned a JSON 404.
  it("serves HTML at the root rather than a JSON 404", async () => {
    const res = await get("/");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("text/html");
    expect(await res.text()).toContain("r2backup");
  });

  it("serves the logo through the router", async () => {
    const res = await get("/logo.png");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("image/png");
  });

  it("leaves the API routes alone", async () => {
    const res = await get("/health");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("application/json");
  });

  it("still 404s an unknown path", async () => {
    expect((await get("/nope")).status).toBe(404);
  });

  // Static public pages; a POST to one is not a thing.
  it("does not answer a POST to the pages", async () => {
    expect((await get("/", "POST")).status).toBe(404);
    expect((await get("/logo.png", "POST")).status).toBe(404);
  });
});

describe("what the landing page tells a wary reader", () => {
  // Someone arriving here has just been asked to grant an unfamiliar program
  // access to their Cloudflare account and is deciding whether to. Each of
  // these is a question they are actually asking.
  const body = () => handleLanding().text();

  it("names the access it asks for", async () => {
    const html = await body();
    expect(html).toContain("Workers R2 Storage");
    expect(html).toContain("Memberships");
  });

  it("says the scopes that could read their files are not requested", async () => {
    const html = (await body()).toLowerCase();
    expect(html).toContain("does not request them");
  });

  it("says the sign-in does not outlive setup", async () => {
    const html = (await body()).toLowerCase();
    expect(html).toContain("revoked");
  });

  it("says how to withdraw the access", async () => {
    expect(await body()).toContain("Authorized Applications");
  });

  it("links to the source", async () => {
    expect(await body()).toContain("github.com/saurabhhbansal/r2backup");
  });

  // Claiming a relationship with Cloudflare that does not exist is the one
  // thing a page behind a verified badge really must not do.
  it("disclaims any affiliation with Cloudflare", async () => {
    const html = (await body()).toLowerCase();
    expect(html).toContain("not affiliated");
  });
});

describe("the logo", () => {
  it("decodes to a real PNG", async () => {
    const bytes = new Uint8Array(await handleLogo().arrayBuffer());
    expect(bytes.length).toBeGreaterThan(1000);
    // The PNG magic number, so a mangled base64 constant fails here rather
    // than as a broken image on a consent screen.
    expect(Array.from(bytes.slice(0, 8))).toEqual([137, 80, 78, 71, 13, 10, 26, 10]);
  });
});
