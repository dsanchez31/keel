/**
 * Tells Cesium where its workers and assets are served (vite.config.ts copies
 * them under `/cesium`).
 *
 * A module of its own, imported first by src/main.tsx, rather than a
 * statement in main.tsx: imports are hoisted, so a statement in main.tsx runs
 * after every module main.tsx imports has been evaluated, Cesium included.
 * Side-effect imports are evaluated in order, so this one runs before any
 * Cesium module does.
 */
window.CESIUM_BASE_URL = __CESIUM_BASE_URL__;
