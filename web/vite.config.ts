import { fileURLToPath, URL } from 'node:url';

import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { viteStaticCopy } from 'vite-plugin-static-copy';
// `vitest/config` re-exports Vite's defineConfig widened with the `test` key.
import { defineConfig } from 'vitest/config';

/**
 * Cesium ships its runtime assets (web workers, imagery and model assets,
 * widget CSS) as static files served next to the bundle. They are copied under
 * `/cesium`, which is what `window.CESIUM_BASE_URL` points at
 * (src/cesium/baseUrl.ts).
 */
const CESIUM_SOURCE = 'node_modules/cesium/Build/Cesium';
const CESIUM_BASE_URL = 'cesium';

/**
 * How many leading path segments viteStaticCopy must drop so a copied asset
 * lands at `/cesium/<Dir>/...` rather than at `/cesium/<the whole source
 * path>/...`.
 *
 * Load bearing since vite-plugin-static-copy v4: it joins `dest` with the
 * matched file's full relative directory, where v2 joined it with the path
 * below `src`. Without this the assets end up under
 * `dist/cesium/node_modules/cesium/Build/Cesium/...`, every worker and every
 * tile 404s, and Cesium halts with "An error occurred while rendering", a
 * message that names neither the file nor the URL.
 */
const CESIUM_SOURCE_DEPTH = CESIUM_SOURCE.split('/').length;

/**
 * Where the dev server and `vite preview` forward `/api`: keeld's listen
 * address in examples/keeld/*.yaml unless KEELD_URL says otherwise.
 *
 * The proxy keeps the browser's Host header (no `changeOrigin`). keeld guards
 * its state-changing routes with http.CrossOriginProtection and its WebSocket
 * with an origin check, both comparing Origin with Host: rewriting Host to
 * keeld's own address would make every POST and the stream look cross-origin.
 */
const KEELD_URL = process.env.KEELD_URL || 'http://127.0.0.1:8080';

const proxy = {
  '/api': { target: KEELD_URL, ws: true },
};

export default defineConfig({
  plugins: [
    react(),
    // Tailwind v4 compiles the theme declared in src/index.css; there is no
    // PostCSS step and no tailwind.config.ts.
    tailwindcss(),
    viteStaticCopy({
      targets: ['Workers', 'Assets', 'Widgets', 'ThirdParty'].map((dir) => ({
        src: `${CESIUM_SOURCE}/${dir}`,
        dest: CESIUM_BASE_URL,
        rename: { stripBase: CESIUM_SOURCE_DEPTH },
      })),
    }),
  ],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  define: {
    // Read by src/cesium/baseUrl.ts, evaluated before any Cesium module.
    __CESIUM_BASE_URL__: JSON.stringify(`/${CESIUM_BASE_URL}`),
  },
  // 5173 is the origin examples/keeld/*.yaml trust. strictPort fails on a busy
  // port instead of moving to another origin nobody listed.
  server: { port: 5173, strictPort: true, proxy },
  preview: { proxy },
  build: {
    // Cesium is a large, self-contained dependency: its own chunk keeps the
    // application chunk small and cacheable across releases.
    //
    // A codeSplitting group, not manualChunks: Vite 8 bundles with Rolldown,
    // which marks manualChunks deprecated. Groups match on module id, so the
    // entry names the packages behind `cesium` as well (`@cesium/engine`,
    // `@cesium/widgets`, protobufjs); one missed falls back into the
    // application chunk without a word. `[\\/]` because ids carry native
    // separators on Windows.
    rollupOptions: {
      output: {
        codeSplitting: {
          groups: [
            {
              name: 'cesium',
              test: /node_modules[\\/](@cesium[\\/]|cesium[\\/]|protobufjs[\\/])/,
            },
          ],
        },
      },
    },
    chunkSizeWarningLimit: 4000,
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    css: false,
  },
});
