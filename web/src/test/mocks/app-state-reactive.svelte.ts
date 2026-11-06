// A reactive stand-in for `$app/state`'s `page`, for the few specs that must
// watch a component react to a change of `page.url` / `page.shallow` (the plain
// object in app-state.ts is not reactive). Used from a spec's `vi.mock` factory.
type Shallow = { url: URL; route: { id: string } | null; params: Record<string, string> | null } | null;

export const page = $state({
	params: {} as Record<string, string>,
	url: new URL('http://localhost/'),
	route: { id: null as string | null },
	status: 200,
	error: null as unknown,
	data: {} as Record<string, unknown>,
	form: null as unknown,
	state: {} as Record<string, unknown>,
	shallow: null as Shallow,
});

export const navigating = { from: null, to: null, type: null, complete: null };
export const updated = { current: false, check: async () => false };
