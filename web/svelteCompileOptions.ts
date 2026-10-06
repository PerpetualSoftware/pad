// The Svelte compile options every .svelte file in this app is built with,
// shared by the two configs that compile Svelte (TASK-3423):
//
//   - vite.config.ts passes them to `sveltekit({...})`, which forwards them to
//     vite-plugin-svelte for the app build and the dev server;
//   - vitest.config.ts passes them to the jsdom project's own `svelte()`.
//
// They used to live in svelte.config.js, which vite-plugin-svelte loads by
// itself, and that is how the jsdom project got them without naming them.
// SvelteKit 3 refuses that file, and deleting it would have dropped runes mode
// from component tests silently. Both consumers now import them from here, and
// runesMode.svelte.test.ts / svelteCompileOptions.test.ts fail if either stops.

/** Every file of ours compiles in runes mode; dependencies decide for themselves. */
export function dynamicCompileOptions({ filename }: { filename: string }): { runes: true } | undefined {
	return filename.includes('node_modules') ? undefined : { runes: true };
}
