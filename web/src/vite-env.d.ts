/** Injected by `define` in vite.config.ts: the public path Cesium's assets are served from. */
declare const __CESIUM_BASE_URL__: string;

interface Window {
  /** Where Cesium resolves its workers and assets, set by src/cesium/baseUrl.ts. */
  CESIUM_BASE_URL: string;
}
