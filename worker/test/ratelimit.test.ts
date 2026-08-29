import { describe, it, expect } from "vitest";
import { evaluateRateLimit, checkRateLimit, type RateLimitState } from "../src/ratelimit";

describe("evaluateRateLimit (pure)", () => {
  it("allows the first request in a fresh window", () => {
    const decision = evaluateRateLimit(null, 1000, 600, 3);
    expect(decision.allowed).toBe(true);
    expect(decision.state).toEqual({ windowStart: 1000, count: 1 });
  });

  it("allows requests under the limit within the same window", () => {
    const state: RateLimitState = { windowStart: 1000, count: 1 };
    const decision = evaluateRateLimit(state, 1050, 600, 3);
    expect(decision.allowed).toBe(true);
    expect(decision.state).toEqual({ windowStart: 1000, count: 2 });
  });

  it("denies once the limit is reached within the window", () => {
    const state: RateLimitState = { windowStart: 1000, count: 3 };
    const decision = evaluateRateLimit(state, 1050, 600, 3);
    expect(decision.allowed).toBe(false);
    expect(decision.retryAfterSeconds).toBe(1000 + 600 - 1050);
  });

  it("starts a fresh window once the old one has expired", () => {
    const state: RateLimitState = { windowStart: 1000, count: 3 };
    const decision = evaluateRateLimit(state, 1601, 600, 3);
    expect(decision.allowed).toBe(true);
    expect(decision.state).toEqual({ windowStart: 1601, count: 1 });
  });

  it("treats exactly the window boundary as expired", () => {
    const state: RateLimitState = { windowStart: 1000, count: 3 };
    const decision = evaluateRateLimit(state, 1600, 600, 3);
    expect(decision.allowed).toBe(true);
  });
});

describe("checkRateLimit (store-backed)", () => {
  function fakeStore() {
    const data = new Map<string, RateLimitState>();
    return {
      data,
      async getRateLimit(key: string) {
        return data.get(key) ?? null;
      },
      async setRateLimit(key: string, state: RateLimitState) {
        data.set(key, state);
      },
    };
  }

  it("persists the decision state back to the store", async () => {
    const store = fakeStore();
    await checkRateLimit(store, "email:a@b.com", 1000, 600, 3);
    await checkRateLimit(store, "email:a@b.com", 1010, 600, 3);
    expect(store.data.get("email:a@b.com")).toEqual({ windowStart: 1000, count: 2 });
  });

  it("denies the request once the per-key limit is exhausted", async () => {
    const store = fakeStore();
    let last;
    for (let i = 0; i < 4; i++) {
      last = await checkRateLimit(store, "ip:1.2.3.4", 1000 + i, 600, 3);
    }
    expect(last?.allowed).toBe(false);
  });

  it("keeps separate counters per key", async () => {
    const store = fakeStore();
    await checkRateLimit(store, "email:a@b.com", 1000, 600, 1);
    const other = await checkRateLimit(store, "email:c@d.com", 1000, 600, 1);
    expect(other.allowed).toBe(true);
  });
});
