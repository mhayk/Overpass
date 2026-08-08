import { cp, mkdir, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Copy Cesium's runtime assets into public/cesium.
 *
 * Cesium is not a normal npm package. It ships web workers, WebAssembly and
 * textures that it fetches by URL at runtime, and no bundler can inline them —
 * they have to sit at a path the browser can reach, which CESIUM_BASE_URL names.
 *
 * Getting this wrong produces a globe that renders a black sphere and logs
 * nothing useful, which is a genuinely unpleasant afternoon. Hence a postinstall
 * step rather than a line in a README nobody reads.
 */
const here = dirname(fileURLToPath(import.meta.url));
const source = join(here, '..', 'node_modules', 'cesium', 'Build', 'Cesium');
const destination = join(here, '..', 'public', 'cesium');

if (!existsSync(source)) {
  console.error(`cesium assets not found at ${source} — is cesium installed?`);
  process.exit(1);
}

// Removed first, so a Cesium upgrade cannot leave a stale worker behind that
// disagrees with the version doing the importing.
await rm(destination, { recursive: true, force: true });
await mkdir(destination, { recursive: true });
await cp(source, destination, { recursive: true });

console.log(`cesium assets copied to public/cesium`);
