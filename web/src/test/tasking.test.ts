import { afterEach, describe, expect, it, vi } from 'vitest';

import { SubmitRejected, idempotencyKey, submitRequest } from '@/lib/tasking';

const REQUEST = {
  customerId: 'acme-imaging',
  targetName: 'Rotterdam',
  target: { type: 'Point', coordinates: [4.4, 51.9] },
  windowStart: '2026-03-01T00:00:00Z',
  windowEnd: '2026-03-02T00:00:00Z',
  priorityTier: 'BEST_EFFORT' as const,
  bidCredits: 0,
  requestedModes: ['SCAN' as const],
};

function accepted(headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify({ request_id: 'r1', state: 'RECEIVED' }), {
    status: 202,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

afterEach(() => vi.unstubAllGlobals());

describe('the idempotency key', () => {
  it('is stable for the same body, so a double-click is harmless', () => {
    expect(idempotencyKey({ a: 1 })).toBe(idempotencyKey({ a: 1 }));
  });

  it('differs for a different body, so a changed form is a new request', () => {
    // The same key with a changed body is a 409 from the API — correctly.
    // Deriving the key from the body is what keeps a client from provoking one.
    expect(idempotencyKey({ a: 1 })).not.toBe(idempotencyKey({ a: 2 }));
  });

  it('is shaped like a v4 uuid, which the API validates', () => {
    expect(idempotencyKey({ a: 1 })).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-8[0-9a-f]{3}-[0-9a-f]{12}$/,
    );
  });
});

describe('submission', () => {
  it('reads the replay marker from the header, not the status', async () => {
    // A replay and a new acceptance are both 202: from the caller's point of
    // view the outcome is identical. Only the header tells them apart.
    vi.stubGlobal('fetch', vi.fn(async () => accepted({ 'Idempotency-Replayed': 'true' })));

    await expect(submitRequest(REQUEST)).resolves.toMatchObject({ replayed: true });
  });

  it('treats a plain 202 as a new acceptance', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => accepted()));

    await expect(submitRequest(REQUEST)).resolves.toMatchObject({ replayed: false });
  });

  it('surfaces every field error, not just the first', async () => {
    // The API renders all of them on purpose, so a user fixes one form rather
    // than four. Throwing the list away here would undo that.
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              title: 'Request failed validation',
              detail: 'see errors',
              errors: [
                { pointer: '/target_name', message: 'target_name is required' },
                { pointer: '/bid_credits', message: 'bid_credits must not be negative' },
              ],
            }),
            { status: 422, headers: { 'Content-Type': 'application/json' } },
          ),
      ),
    );

    await expect(submitRequest(REQUEST)).rejects.toBeInstanceOf(SubmitRejected);
    await expect(submitRequest(REQUEST)).rejects.toMatchObject({
      fieldErrors: [{ pointer: '/target_name' }, { pointer: '/bid_credits' }],
    });
  });

  it('sends the derived key as a header', async () => {
    const spy = vi.fn(async () => accepted());
    vi.stubGlobal('fetch', spy);

    await submitRequest(REQUEST);

    // The mock is typed from the zero-argument stub, so the call tuple has no
    // index 1 as far as TypeScript is concerned. Widened rather than cast away:
    // `as RequestInit` on an out-of-range index is a lie the compiler was right
    // to refuse.
    const calls = spy.mock.calls as unknown as Array<[string, RequestInit]>;
    const init = calls[0]?.[1];
    const headers = init?.headers as Record<string, string> | undefined;
    expect(headers?.['Idempotency-Key']).toMatch(/^[0-9a-f]{8}-/);
  });
});
