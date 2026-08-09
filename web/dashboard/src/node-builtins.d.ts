// svelte-check runs with `types: []` (see tsconfig.json), so Node's built-ins
// are unresolved even though vitest executes tests in a node environment. The
// theme-parity test reads app.css straight from disk, because the tailwind vite
// plugin rewrites .css imports even with ?raw. Declaring only the handful of
// members that test uses keeps @types/node out of the dependency list.
declare module 'node:fs' {
  export function readFileSync(path: string, encoding: string): string;
}

declare module 'node:path' {
  const path: {
    join(...parts: string[]): string;
    dirname(p: string): string;
  };
  export default path;
}

declare module 'node:url' {
  export function fileURLToPath(url: string | URL): string;
}
