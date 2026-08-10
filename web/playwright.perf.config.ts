import { defineConfig, devices } from '@playwright/test';

/**
 * The frontend performance harness (M4-07), deliberately NOT the E2E suite.
 *
 * Its own config and its own directory because it is measured in minutes, not
 * seconds: the memory criterion is "over a 10-minute session", and leaks in a
 * live-updating visualisation do not show up in a 30-second look. Putting it in
 * e2e/ would either make every CI run ten minutes longer or make someone add a
 * skip, and a skipped measurement is worse than none because it still looks
 * like coverage.
 *
 * Run it with scripts/frontend-perf.sh, against a stack with data in it.
 */
export default defineConfig({
  testDir: './perf',
  // Named *.perf.ts rather than *.spec.ts, so the default glob cannot pick
  // these up if someone points the E2E config at a wider directory later.
  testMatch: '**/*.perf.ts',
  // 15 minutes. The session under measurement is 10, and the setup before it
  // has to fit inside the same budget.
  timeout: 15 * 60_000,
  expect: { timeout: 60_000 },
  // One worker and no retries: a second browser competing for the GPU would be
  // measuring the harness rather than the app, and a retried run silently
  // reports the luckier of two samples.
  workers: 1,
  retries: 0,
  fullyParallel: false,
  reporter: [['list']],
  use: {
    baseURL: process.env.WEB_URL ?? 'http://localhost:3000',
    ...devices['Desktop Chrome'],
    // Traces and video would themselves cost frames. The whole point here is
    // the frame times, so the instrument stays out of the measurement.
    trace: 'off',
    video: 'off',
    screenshot: 'off',
  },
});
