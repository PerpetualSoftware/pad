// A REACTIVE `$app/state` page double, for suites that navigate between routes
// while a component is mounted. The default alias (`app-state.ts`) is a plain
// object, so reassigning `page.params` there never re-runs a `$derived` that
// reads it, and a "switch workspace mid-load" leg silently measures nothing
// (BUG-3237: the first run of its workspace-switch leg issued no second load).
//
// Use from a suite with:
//   vi.mock('$app/state', async () => ({ page: (await import('<path>/reactivePage.svelte')).page }));
// `$state` must live at module scope in a `.svelte.ts` file; a hoisted mock
// factory runs before the Svelte runtime is ready to declare it.

export const page = $state({
	params: {} as Record<string, string>,
	url: new URL('http://localhost/'),
	route: { id: null as string | null },
	status: 200,
	error: null as unknown,
	data: {} as Record<string, unknown>,
	form: null as unknown,
	state: {} as Record<string, unknown>,
});
