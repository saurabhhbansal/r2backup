import { describe, it, expect } from "vitest";
import { readJsonBody } from "../src/http";

describe("readJsonBody", () => {
  it("parses a small valid body", async () => {
    const req = new Request("https://x/", { method: "POST", body: JSON.stringify({ a: 1 }) });
    expect(await readJsonBody(req, 1024)).toEqual({ a: 1 });
  });

  it("rejects a body over the byte limit even without a Content-Length header", async () => {
    const big = JSON.stringify({ a: "x".repeat(1000) });
    const req = new Request("https://x/", { method: "POST", body: big });
    expect(await readJsonBody(req, 10)).toBeNull();
  });

  it("rejects malformed JSON", async () => {
    const req = new Request("https://x/", { method: "POST", body: "{not json" });
    expect(await readJsonBody(req, 1024)).toBeNull();
  });
});
