// Vite's `?raw` import, which Vitest serves: a file's text as a string. The
// tests read their fixtures so, the package declaring no Node types.
declare module '*?raw' {
  const text: string;
  export default text;
}
