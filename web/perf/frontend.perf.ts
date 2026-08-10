import { expect, test } from '@playwright/test';

import { GATEWAY, TASKING, openWorkspace, submit } from '../e2e/support';

/**
 * The M4-07 measurement: frame time, time to interactive, memory over ten
 * minutes.
 *
 * MEASURED, NOT FELT. "It feels smooth on my machine" is exactly the claim that
 * collapses on a reviewer's laptop, and the numbers this writes are the ones
 * docs/performance.md publishes. Nothing here asserts a threshold except the
 * two that would represent a real regression — a leak, and a frozen frame —
 * because a performance test that fails on noise gets deleted.
 *
 * Frame times come from requestAnimationFrame deltas in the page. Heap, node
 * and listener counts come from CDP, because a heap that is flat while DOM
 * nodes climb is still a leak, and heap alone would miss it.
 */

interface Sample {
  atS: number;
  heapMB: number;
  nodes: number;
  listeners: number;
  documents: number;
}

const SESSION_MINUTES = Number(process.env.PERF_MINUTES ?? 10);
const SAMPLE_EVERY_MS = 30_000;
// One submission a minute. Enough to keep the SSE stream and the projector
// working — which is the state a leak would show up in — without outrunning
// feasibility's single worker at roughly 0.25 rps (#189).
const SUBMIT_EVERY_MS = 60_000;

test('frame time, time to interactive, and memory over a session', async ({ page, request }, testInfo) => {
  const client = await page.context().newCDPSession(page);
  await client.send('Performance.enable');
  await client.send('HeapProfiler.enable');

  // GC BEFORE MEASURING GROWTH, or "growth" is just garbage nobody collected
  // yet. Without this the leak assertion below would fire on a healthy page and
  // stay silent on a real leak the day V8 happened to collect at the right
  // moment.
  const collectGarbage = async (): Promise<void> => {
    await client.send('HeapProfiler.collectGarbage');
    await page.waitForTimeout(500);
  };

  const metrics = async (): Promise<Omit<Sample, 'atS'>> => {
    const { metrics: raw } = await client.send('Performance.getMetrics');
    const get = (name: string): number => raw.find((m) => m.name === name)?.value ?? 0;
    return {
      heapMB: get('JSHeapUsedSize') / 1048576,
      nodes: get('Nodes'),
      listeners: get('JSEventListeners'),
      documents: get('Documents'),
    };
  };

  // ---- Time to interactive -------------------------------------------------
  //
  // Measured to the first rendered acquisition, not to load. A page that has
  // painted its chrome and cannot yet show a row is not interactive in any
  // sense the user cares about — and this app's first row requires the bundle,
  // hydration, and a cross-origin fetch to complete.
  const startedAt = Date.now();
  await openWorkspace(page);
  await page.getByTestId('acquisition').first().waitFor({ state: 'visible' });
  const ttiMs = Date.now() - startedAt;

  const navigation = await page.evaluate(() => {
    const nav = performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming | undefined;
    return {
      domInteractiveMs: nav?.domInteractive ?? 0,
      domContentLoadedMs: nav?.domContentLoadedEventEnd ?? 0,
      loadMs: nav?.loadEventEnd ?? 0,
      transferredKB: (nav?.transferSize ?? 0) / 1024,
    };
  });

  // The globe is the expensive one and arrives after first paint by design.
  const globeStartedAt = Date.now();
  await page
    .locator('[data-testid="cesium-container"] canvas')
    .first()
    .waitFor({ state: 'attached' });
  const globeReadyMs = Date.now() - globeStartedAt;

  const afterLoad = await metrics();

  // ---- Frame time under interaction ---------------------------------------
  //
  // Sampled during real work — selecting acquisitions, which is what used to
  // rebuild the entire WebGL context. An idle page renders nothing and would
  // report a flattering number about a state nobody is in.
  // NEITHER OF THESE IS AN ABSOLUTE NUMBER ABOUT THE PRODUCT.
  //
  // This runs headless on software WebGL in a container with no GPU, so Cesium
  // rasterises on the CPU — on the MAIN THREAD. That inflates frame times and
  // it inflates long tasks too, which is the correction to a comment that used
  // to claim long tasks were hardware-independent. They are not: measured
  // before and after the entity-sync rewrite, total long-task time went UP,
  // because the old version spent part of every interaction with no viewer at
  // all and a torn-down context renders nothing.
  //
  // What these are good for is a BEFORE/AFTER on the same environment, with
  // the same interaction. Read them that way; read the memory series, the node
  // and listener counts, and the document count as the hardware-independent
  // evidence.
  await page.evaluate(() => {
    const w = window as unknown as { __longTasks?: number[] };
    w.__longTasks = [];
    try {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          w.__longTasks?.push(entry.duration);
        }
      }).observe({ entryTypes: ['longtask'] });
    } catch {
      // Not every build exposes longtask. Absent is reported as absent rather
      // than as zero, below.
    }
  });

  await page.evaluate(() => {
    const w = window as unknown as { __frames?: number[] };
    w.__frames = [];
    let last = performance.now();
    const tick = (now: number): void => {
      w.__frames?.push(now - last);
      last = now;
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });

  const rows = page.getByTestId('acquisition');
  const clicks = Math.min(await rows.count(), 8);
  expect(clicks, 'no acquisitions on the page; seed the stack before measuring').toBeGreaterThan(0);
  for (let i = 0; i < clicks; i++) {
    await rows.nth(i).click();
    await page.waitForTimeout(700);
  }

  const frames = await page.evaluate(() => {
    const w = window as unknown as { __frames?: number[] };
    return (w.__frames ?? []).slice();
  });
  const sorted = frames.slice().sort((a, b) => a - b);
  const pct = (p: number): number => sorted[Math.min(sorted.length - 1, Math.floor((sorted.length * p) / 100))] ?? 0;
  const frameStats = {
    count: frames.length,
    p50: pct(50),
    p95: pct(95),
    worst: sorted[sorted.length - 1] ?? 0,
    // 16.7ms is the 60fps budget. Kept for completeness and NOT publishable
    // from this environment: under software rasterisation essentially every
    // frame is over budget, so the figure says what the container is, not what
    // the application is. On a GPU it would be the honest way to state "60fps",
    // because a p50 under budget with a long tail still stutters visibly.
    overBudgetPercent: frames.length ? (frames.filter((f) => f > 16.7).length / frames.length) * 100 : 0,
  };

  const longTasks = await page.evaluate(() => {
    const w = window as unknown as { __longTasks?: number[] };
    return w.__longTasks ? w.__longTasks.slice() : null;
  });
  const longTaskStats = longTasks === null
    ? { supported: false, count: 0, totalMs: 0, worstMs: 0 }
    : {
        supported: true,
        count: longTasks.length,
        totalMs: longTasks.reduce((sum, d) => sum + d, 0),
        worstMs: longTasks.reduce((max, d) => Math.max(max, d), 0),
      };

  await collectGarbage();
  const afterInteraction = await metrics();

  // ---- Memory over the session --------------------------------------------
  const samples: Sample[] = [];
  const sessionStart = Date.now();
  let lastSubmit = 0;

  while (Date.now() - sessionStart < SESSION_MINUTES * 60_000) {
    if (Date.now() - lastSubmit > SUBMIT_EVERY_MS) {
      lastSubmit = Date.now();
      // Through the real ingress, so the projector folds, the read model
      // changes, and the SSE stream pushes — the live-updating state this
      // measurement exists to hold under observation.
      await submit(request, {
        customerId: 'acme-imaging',
        targetName: `perf session ${Date.now()}`,
        bidCredits: 300,
      }).catch(() => undefined);
    }
    await page.waitForTimeout(SAMPLE_EVERY_MS);
    await collectGarbage();
    samples.push({ atS: Math.round((Date.now() - sessionStart) / 1000), ...(await metrics()) });
  }

  const first = samples[0];
  const last = samples[samples.length - 1];
  const growth = first && last
    ? {
        heapMB: last.heapMB - first.heapMB,
        nodes: last.nodes - first.nodes,
        listeners: last.listeners - first.listeners,
        documents: last.documents - first.documents,
      }
    : { heapMB: 0, nodes: 0, listeners: 0, documents: 0 };

  const report = {
    sessionMinutes: SESSION_MINUTES,
    gateway: GATEWAY,
    tasking: TASKING,
    tti: { firstAcquisitionMs: ttiMs, globeReadyMs, ...navigation },
    // Reported with its confound attached. See the comment above the observer:
    // software rasterisation in a container is what this number measures.
    frames: { ...frameStats, environment: 'headless, software WebGL, no GPU' },
    longTasks: longTaskStats,
    heap: { afterLoad, afterInteraction, samples, growth },
  };

  // Written as an artefact rather than only logged, because docs/performance.md
  // quotes these numbers and a number retyped from a terminal is a number that
  // drifts from the run that produced it.
  await testInfo.attach('frontend-performance.json', {
    body: JSON.stringify(report, null, 2),
    contentType: 'application/json',
  });
  console.log('PERF_REPORT ' + JSON.stringify(report));

  // ---- The only two assertions --------------------------------------------
  //
  // A DOCUMENT LEAK IS THE ONE THAT MATTERS HERE. Each rebuilt Cesium viewer
  // used to take a WebGL context with it; detached documents accumulating over
  // a session is the signature of exactly that class of bug, and unlike heap it
  // is not noisy.
  expect(growth.documents, 'documents accumulated over the session; something is not being torn down')
    .toBeLessThanOrEqual(2);

  // A frame budget blown by an order of magnitude is a freeze, not jitter.
  // Deliberately loose: this is a regression tripwire, not a target.
  expect(frameStats.worst, 'a single frame took longer than a second; the page froze').toBeLessThan(1000);
});
