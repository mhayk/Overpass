import { defineConfig, devices } from '@playwright/test';

/**
 * Two paths, end to end, in a real browser against the real stack.
 *
 * NOT a general test layer. E2E is the slowest and most brittle rung, and it
 * earns its place only on paths that cross every boundary — ingress, broker,
 * SGP4, planner, projector, read model, browser. Everything else belongs lower
 * in the pyramid, where it runs in milliseconds and fails for one reason.
 *
 * The stack is expected to be UP already (`make up-all && make seed`). Starting
 * it here would mean this config owning container lifecycle as well as browser
 * lifecycle, and a failure would be ambiguous between the two.
 */
export default defineConfig({
  testDir: './e2e',

  // NO ARBITRARY SLEEPS ANYWHERE, which is why these budgets are generous
  // instead. The pipeline is genuinely slow — one feasibility worker doing
  // SGP4 across nine satellites takes ~4s per request, and a planning round
  // waits out a quiet period — so a test that waits on the real condition has
  // to be allowed to wait. Waiting on a condition is deterministic however
  // long it takes; sleeping is not, however short.
  timeout: 180_000,
  expect: { timeout: 60_000 },

  // Serial. Two browsers submitting into one constellation would contend for
  // the same passes, which is the very thing the contested test measures — the
  // suite would then be testing itself.
  workers: 1,
  fullyParallel: false,

  // Retries hide flakiness, and flakiness here means a real race in the
  // system. A red build that reruns green teaches people to press the button.
  retries: 0,

  reporter: process.env.CI ? [['github'], ['list']] : [['list']],

  use: {
    baseURL: process.env.WEB_URL ?? 'http://localhost:3000',
    // Captured ON FAILURE only. A trace per run is gigabytes nobody opens; a
    // trace for the run that broke is the one artefact worth having.
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
