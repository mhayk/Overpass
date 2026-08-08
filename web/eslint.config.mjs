import js from '@eslint/js';
import nextPlugin from '@next/eslint-plugin-next';
import tseslint from 'typescript-eslint';

/**
 * Flat config, with the Next PLUGIN rather than the eslint-config-next preset.
 *
 * The preset still loads @rushstack/eslint-patch, which fails outright under
 * flat config on ESLint 9 — "Failed to patch ESLint because the calling module
 * was not recognized". The plugin carries the same rules without the patch.
 */
export default tseslint.config(
  { ignores: ['.next/**', 'node_modules/**', 'public/cesium/**', 'next-env.d.ts'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    plugins: { '@next/next': nextPlugin },
    rules: {
      ...nextPlugin.configs.recommended.rules,
      ...nextPlugin.configs['core-web-vitals'].rules,

      // `any` is banned outright rather than warned about. The contracts are
      // generated and typed end to end; an `any` here is a place where that
      // guarantee silently stops.
      '@typescript-eslint/no-explicit-any': 'error',
      '@typescript-eslint/consistent-type-imports': 'error',
    },
  },
  {
    // The build scripts run under Node, not in a browser. Without this,
    // `console` and `process` are undefined globals — correctly, for app code.
    files: ['scripts/**/*.mjs', '*.config.{mjs,ts}'],
    languageOptions: {
      globals: { console: 'readonly', process: 'readonly' },
    },
  },
);
