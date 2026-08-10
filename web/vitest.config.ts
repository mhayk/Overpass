import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    // Cesium is excluded from the unit environment on purpose: it needs WebGL,
    // which jsdom does not have. What is testable without a GPU — the API
    // client, the CZML/GeoJSON shaping, the state around selection — is tested
    // here; the globe itself is a Playwright concern in M4-08.
    // e2e/ is Playwright's, and its specs import a `test` that refuses to run
    // under any other runner: "Playwright Test did not expect test() to be
    // called here." Two suites sharing a `*.spec.ts` suffix need the boundary
    // stated, or `vitest run` reports two failures that are not tests failing.
    exclude: ['node_modules/**', '.next/**', 'e2e/**'],
  },
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
});
