import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { subscribe, type LiveChange } from '@/lib/live';

/** A minimal EventSource stand-in: jsdom has none. */
class FakeEventSource {
  static last: FakeEventSource | undefined;
  listeners = new Map<string, ((event: MessageEvent) => void)[]>();
  closed = false;

  constructor(readonly url: string) {
    FakeEventSource.last = this;
  }

  addEventListener(type: string, handler: (event: MessageEvent) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), handler]);
  }

  close(): void {
    this.closed = true;
  }

  emit(type: string, data: unknown): void {
    for (const handler of this.listeners.get(type) ?? []) {
      handler({ data: JSON.stringify(data) } as MessageEvent);
    }
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('subscribe', () => {
  it('coalesces a burst into one delivery', () => {
    // A planning round commits many changes at once. Handing each to setState
    // turns one logical update into dozens of renders — the thing M4-07 asks
    // to be prevented before it is measured.
    const batches: LiveChange[][] = [];
    subscribe({ gatewayUrl: 'http://gw', onChanges: (c) => batches.push(c) });

    for (let i = 0; i < 30; i++) {
      FakeEventSource.last!.emit('request', { request_id: `r${i}`, state: 'RECEIVED' });
    }
    expect(batches).toHaveLength(0); // nothing delivered yet

    vi.advanceTimersByTime(200);
    expect(batches).toHaveLength(1);
    expect(batches[0]).toHaveLength(30);
  });

  it('delivers a reset immediately, ahead of anything coalesced', () => {
    // A reset means "you may have missed something". Holding it alongside
    // changes it cannot vouch for would let a subscriber act on a partial
    // picture first.
    const batches: LiveChange[][] = [];
    subscribe({ gatewayUrl: 'http://gw', onChanges: (c) => batches.push(c) });

    FakeEventSource.last!.emit('request', { request_id: 'r1' });
    FakeEventSource.last!.emit('reset', {});

    expect(batches).toHaveLength(1);
    expect(batches[0]![0]!.kind).toBe('reset');
  });

  it('drops the pending batch on reset rather than delivering stale changes', () => {
    const batches: LiveChange[][] = [];
    subscribe({ gatewayUrl: 'http://gw', onChanges: (c) => batches.push(c) });

    FakeEventSource.last!.emit('request', { request_id: 'r1' });
    FakeEventSource.last!.emit('reset', {});
    vi.advanceTimersByTime(500);

    // Only the reset. The coalesced change was superseded by "refetch".
    expect(batches).toHaveLength(1);
  });

  it('survives a frame it cannot parse', () => {
    // A dropped connection costs more than a dropped event.
    const batches: LiveChange[][] = [];
    subscribe({ gatewayUrl: 'http://gw', onChanges: (c) => batches.push(c) });

    const source = FakeEventSource.last!;
    for (const handler of source.listeners.get('request') ?? []) {
      handler({ data: 'not json' } as MessageEvent);
    }
    source.emit('request', { request_id: 'r1' });
    vi.advanceTimersByTime(200);

    expect(batches).toHaveLength(1);
    expect(batches[0]).toHaveLength(1);
  });

  it('passes filters through as query parameters', () => {
    subscribe({ gatewayUrl: 'http://gw', requestId: 'r1', onChanges: () => {} });
    expect(FakeEventSource.last!.url).toContain('request_id=r1');
  });

  it('closes the stream when unsubscribed', () => {
    const stop = subscribe({ gatewayUrl: 'http://gw', onChanges: () => {} });
    stop();
    expect(FakeEventSource.last!.closed).toBe(true);
  });
});
