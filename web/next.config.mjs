/**
 * Cesium is not a normal npm package.
 *
 * It ships a large static asset bundle — workers, web assembly, textures, the
 * widget CSS — that has to be reachable at a known URL at runtime. Bundlers
 * cannot inline it, so the assets are copied into public/cesium at install time
 * and CESIUM_BASE_URL points at them. Getting this wrong produces a globe that
 * renders a black sphere and logs nothing useful.
 */
/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,

  env: {
    // Read by CesiumGlobe before the first Cesium import. Public rather than
    // server-only: the browser is what needs it.
    NEXT_PUBLIC_CESIUM_BASE_URL: '/cesium',
  },

  webpack: (config) => {
    // Cesium's own source uses these Node built-ins in code paths the browser
    // never takes. Left unresolved, the build fails on modules it does not need.
    config.resolve.fallback = { ...config.resolve.fallback, fs: false, path: false };

    // Cesium's exportKml imports `@zip.js/zip.js/lib/zip-no-worker.js`. That
    // file does not exist in the installed version — the package renamed it —
    // so the build fails on a KML export feature nothing here uses:
    //
    //   Module not found: Can't resolve '@zip.js/zip.js/lib/zip-no-worker.js'
    //
    // Aliased to the module that replaced it. Clearing resolve.exportsFields
    // was tried first and is the wrong fix twice over: it changes resolution
    // for every package in the tree to work around one, and it does not help
    // anyway, because the path is genuinely absent rather than merely unexported.
    config.resolve.alias = {
      ...config.resolve.alias,
      '@zip.js/zip.js/lib/zip-no-worker.js': '@zip.js/zip.js/lib/zip-core.js',
    };
    return config;
  },
};

export default nextConfig;
