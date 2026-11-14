// A reactive $app/state stand-in: `page.url` is $state, so a component's effect
// re-runs when a test navigates (TASK-4545).
let url = $state(new URL('http://localhost/'));
export const page = {
	get url() { return url; },
	set url(v: URL) { url = v; },
	params: {} as Record<string, string>,
	state: {} as Record<string, unknown>,
	route: { id: null as string | null },
	shallow: null
};
