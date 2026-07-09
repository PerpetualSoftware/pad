// Test-only stand-in for SvelteKit's `$app/environment`.
//
// The jsdom vitest project runs WITHOUT the SvelteKit vite plugin (it uses the
// plain `@sveltejs/vite-plugin-svelte` plugin so `.svelte` / `.svelte.ts` files
// compile), so `$app/environment` has no provider. vitest.config.ts aliases the
// import to this file for the jsdom project. `browser = true` puts modules like
// `breakpoint.svelte.ts` on their client code path (they guard `matchMedia`
// wiring behind `if (browser)`).
export const browser = true;
export const dev = true;
export const building = false;
export const version = 'test';
