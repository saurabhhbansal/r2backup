// Fixed-window rate limiting backed by D1 (the only storage this Worker has
// -- the Cloudflare token behind it grants Workers Scripts and D1 but no KV
// or Durable Objects, so a counter table is the available option, not a
// stylistic choice).
//
// The windowing math is split out as a pure function so it can be tested
// without touching D1 at all: `evaluateRateLimit` takes whatever state was
// read and returns what should be written back, and the thin D1-backed
// wrapper below just does that one read and one write.

export interface RateLimitState {
  windowStart: number; // seconds since epoch
  count: number;
}

export interface RateLimitDecision {
  allowed: boolean;
  state: RateLimitState;
  retryAfterSeconds: number;
}

export function evaluateRateLimit(
  previous: RateLimitState | null,
  now: number,
  windowSeconds: number,
  maxRequests: number,
): RateLimitDecision {
  if (!previous || now - previous.windowStart >= windowSeconds) {
    return { allowed: true, state: { windowStart: now, count: 1 }, retryAfterSeconds: 0 };
  }
  if (previous.count < maxRequests) {
    return {
      allowed: true,
      state: { windowStart: previous.windowStart, count: previous.count + 1 },
      retryAfterSeconds: 0,
    };
  }
  return {
    allowed: false,
    state: previous,
    retryAfterSeconds: previous.windowStart + windowSeconds - now,
  };
}

export interface RateLimitStore {
  getRateLimit(key: string): Promise<RateLimitState | null>;
  setRateLimit(key: string, state: RateLimitState): Promise<void>;
}

export async function checkRateLimit(
  store: RateLimitStore,
  key: string,
  now: number,
  windowSeconds: number,
  maxRequests: number,
): Promise<RateLimitDecision> {
  const previous = await store.getRateLimit(key);
  const decision = evaluateRateLimit(previous, now, windowSeconds, maxRequests);
  await store.setRateLimit(key, decision.state);
  return decision;
}
