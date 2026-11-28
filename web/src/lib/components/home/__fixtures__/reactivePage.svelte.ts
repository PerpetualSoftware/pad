// A reactive stand-in for `$app/state` (HomePaneHost.svelte.test.ts): the
// host derives the pane from `page.url`, so the test's navigations must be
// observable.
export const page = $state({
	url: new URL('http://localhost/'),
	params: {} as Record<string, string>,
	route: { id: '/(console)' as string | null },
	state: {} as Record<string, unknown>,
	shallow: null as unknown,
	status: 200,
	error: null as unknown,
	data: {} as Record<string, unknown>,
	form: null as unknown,
});
export const navigating = { from: null, to: null, type: null, complete: null };
export const updated = { current: false, check: async () => false };
