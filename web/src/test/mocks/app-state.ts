// Test-only stand-in for SvelteKit's `$app/state`.
//
// Same reason as `app-environment.ts`: the jsdom vitest project runs without
// the SvelteKit vite plugin, so `$app/state` has no provider and any component
// importing it fails to RESOLVE — before `vi.mock` ever gets a chance to
// substitute it. vitest.config.ts aliases the import here so such components
// are mountable at all; a test that cares about the values either mutates the
// exported objects or `vi.mock`s the specifier itself.
//
// Plain mutable objects, not runes: components read `page.params.x` in
// `$derived`, which works fine against a static object for tests that don't
// need the read to be reactive.
export const page = {
	params: {} as Record<string, string>,
	url: new URL('http://localhost/'),
	route: { id: null as string | null },
	status: 200,
	error: null as unknown,
	data: {} as Record<string, unknown>,
	form: null as unknown,
	state: {} as Record<string, unknown>,
};

export const navigating = { from: null, to: null, type: null, complete: null };
export const updated = { current: false, check: async () => false };
